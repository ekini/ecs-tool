package lib

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/apex/log"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/samber/lo"
)

// DeployServices deploys specified services in parallel
func DeployServices(profile, cluster, imageTag string, imageTags, services []string, workDir string, rollbackOnFail, eagerDeployment bool) (exitCode int, err error) {
	ctx := log.WithFields(log.Fields{
		"cluster":   cluster,
		"image_tag": imageTag,
	})

	err = makeConfig(profile)
	if err != nil {
		return 1, err
	}
	exits := make(chan int, len(services))
	rollback := make(chan bool, len(services))

	var wg sync.WaitGroup
	for _, service := range services {
		service := service // go catch
		wg.Add(1)
		go func() {
			defer wg.Done()
			deployService(ctx, cluster, imageTag, imageTags, workDir, service, exits, rollback, eagerDeployment, &wg)
		}()
	}

	for n := 0; n < len(services); n++ {
		if code := <-exits; code > 0 {
			exitCode = 127
			err = fmt.Errorf("One of the services failed to deploy")
		}
	}
	if exitCode != 0 {
		for n := 0; n < len(services); n++ {
			rollback <- rollbackOnFail
		}
	} else {
		close(rollback)
	}

	wg.Wait()
	return
}

func deployService(ctx log.Interface, cluster, imageTag string, imageTags []string, workDir, service string, exitChan chan int, rollback chan bool, eagerDeployment bool, wg *sync.WaitGroup) {
	ctx = ctx.WithFields(log.Fields{
		"service": service,
	})
	ctx.Info("Deploying")

	svc := ecs.NewFromConfig(cfg)

	// first, describe the service to get current task definition
	describeResult, err := svc.DescribeServices(context.TODO(), &ecs.DescribeServicesInput{
		Cluster:  aws.String(cluster),
		Services: []string{service},
	})
	if err != nil {
		ctx.WithError(err).Error("Can't describe service")
		exitChan <- 1
		return
	}
	if len(describeResult.Failures) > 0 {
		for _, failure := range describeResult.Failures {
			ctx.Errorf("%#v", failure)
		}
		exitChan <- 2
		return
	}

	// then describe the task definition to get a copy of it
	describeTaskResult, err := svc.DescribeTaskDefinition(context.TODO(), &ecs.DescribeTaskDefinitionInput{
		TaskDefinition: describeResult.Services[0].TaskDefinition,
	})
	if err != nil {
		ctx.WithError(err).Error("Can't get task definition")
		exitChan <- 3
		return
	}

	taskDefinition := describeTaskResult.TaskDefinition
	// replace the image tag if there is any
	if err := modifyContainerDefinitionImages(imageTag, imageTags, workDir, taskDefinition.ContainerDefinitions, ctx); err != nil {
		ctx.WithError(err).Error("Can't modify container definition images")
		exitChan <- 1
	}

	// now, register the new task
	registerResult, err := svc.RegisterTaskDefinition(context.TODO(), &ecs.RegisterTaskDefinitionInput{
		ContainerDefinitions:    taskDefinition.ContainerDefinitions,
		Cpu:                     taskDefinition.Cpu,
		ExecutionRoleArn:        taskDefinition.ExecutionRoleArn,
		Family:                  taskDefinition.Family,
		Memory:                  taskDefinition.Memory,
		NetworkMode:             taskDefinition.NetworkMode,
		PlacementConstraints:    taskDefinition.PlacementConstraints,
		RequiresCompatibilities: taskDefinition.Compatibilities,
		TaskRoleArn:             taskDefinition.TaskRoleArn,
		Volumes:                 taskDefinition.Volumes,
	})
	if err != nil {
		ctx.WithError(err).Error("Can't register task definition")
		exitChan <- 4
		return
	}
	ctx.WithField(
		"task_definition_arn",
		aws.ToString(registerResult.TaskDefinition.TaskDefinitionArn),
	).Debug("Registered the task definition")

	// now we are running DescribeService periodically to get the events
	doneChan := make(chan bool)
	defer func() { doneChan <- true }()

	wg.Add(1)
	go func(ctx log.Interface, cluster, service string) {
		printedEvents := []string{}
		last := time.Now()

		defer wg.Done()
		svc := ecs.NewFromConfig(cfg)

		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		printEvent := func() {
			describeResult, err := svc.DescribeServices(context.TODO(), &ecs.DescribeServicesInput{
				Cluster:  aws.String(cluster),
				Services: []string{service},
			})
			if err != nil {
				ctx.WithError(err).Error("Can't describe service")
			}
			for _, event := range describeResult.Services[0].Events {
				eventId := aws.ToString(event.Id)
				if !aws.ToTime(event.CreatedAt).Before(last) && !lo.Contains(printedEvents, eventId) {
					ctx.Info(aws.ToString(event.Message))
					printedEvents = lo.Union(printedEvents, []string{eventId})
				}

			}
		}
		for {
			select {
			case <-doneChan:
				printEvent()
				return
			case <-ticker.C:
				printEvent()
			}
		}
	}(ctx, cluster, service)

	// update the service using the new registered task definition
	err = updateService(
		ctx,
		aws.ToString(describeResult.Services[0].ClusterArn),
		aws.ToString(describeResult.Services[0].ServiceArn),
		aws.ToString(registerResult.TaskDefinition.TaskDefinitionArn),
		eagerDeployment,
	)

	wg.Add(1)
	// run the rollback function in background
	go func(ctx log.Interface) {
		defer wg.Done()
		if n, ok := <-rollback; n && ok {
			ctx.WithField(
				"task_definition_arn",
				aws.ToString(describeResult.Services[0].TaskDefinition),
			).Info("Rolling back to the previous task definition")
			if err := updateService(
				ctx,
				aws.ToString(describeResult.Services[0].ClusterArn),
				aws.ToString(describeResult.Services[0].ServiceArn),
				aws.ToString(describeResult.Services[0].TaskDefinition),
				eagerDeployment,
			); err != nil {
				ctx.WithError(err).Error("Couldn't rollback.")
			}
		}
	}(ctx)

	var deregisterTaskArn *string
	if err != nil {
		ctx.WithError(err).Error("Couldn't deploy. Will try to roll back")
		deregisterTaskArn = registerResult.TaskDefinition.TaskDefinitionArn
		exitChan <- 5
	} else {
		deregisterTaskArn = describeTaskResult.TaskDefinition.TaskDefinitionArn
		exitChan <- 0
	}

	// deregister the old task definition
	ctx = ctx.WithFields(log.Fields{"task_definition_arn": aws.ToString(deregisterTaskArn)})
	ctx.Debug("Deregistered the task definition")
	_, err = svc.DeregisterTaskDefinition(context.TODO(), &ecs.DeregisterTaskDefinitionInput{
		TaskDefinition: deregisterTaskArn,
	})
	if err != nil {
		ctx.WithError(err).Error("Can't deregister task definition")
	}
}

func updateService(ctx log.Interface, cluster, service, taskDefinition string, eagerDeployment bool) error {
	svc := ecs.NewFromConfig(cfg)
	// update the service using the new registered task definition
	out, err := svc.UpdateService(context.TODO(), &ecs.UpdateServiceInput{
		Cluster:        aws.String(cluster),
		Service:        aws.String(service),
		TaskDefinition: aws.String(taskDefinition),
	})
	if err != nil {
		ctx.WithError(err).Error("Can't update the service")
		return err
	}
	ctx.Info("Updated the service")

	if eagerDeployment {
		if err := waitUntilDeploymentIsActive(ctx, out); err != nil {
			ctx.WithError(err).Error("Can't wait until service is deployed")
			return err
		}
	} else {
		waiter := ecs.NewServicesStableWaiter(svc)
		err = waiter.Wait(context.TODO(), &ecs.DescribeServicesInput{
			Cluster:  aws.String(cluster),
			Services: []string{service},
		},
			10*time.Minute, // max wait
		)
		if err != nil {
			ctx.WithError(err).Error("The waiter has been finished with an error")
			return err
		}
	}

	ctx.Info("Service has been deployed")
	return nil
}

func waitUntilDeploymentIsActive(ctx log.Interface, updateServiceOutput *ecs.UpdateServiceOutput) error {
	svc := ecs.NewFromConfig(cfg)
	serviceConfigurations := updateServiceOutput.Service.Deployments
	// sort deployments to get the latest one
	sort.SliceStable(serviceConfigurations, func(i, j int) bool {
		return serviceConfigurations[i].CreatedAt.After(aws.ToTime(serviceConfigurations[j].CreatedAt))
	})

	// AWS is weird about output of service update
	// one would expect something that can be used to poll for status right away, but no
	// The output has "deployments", which is a list of task sets, i.e.
	// "ecs-svc/3130987122852940733" - PRIMARY - IN_PROGRESS
	// "ecs-svc/1201712472044336203" - ACTIVE - COMPLETED
	// which is in fact a service configuration revisions
	//
	// The actual deployment to apply that service configuration is retrieved by a different API call and does
	// appear a bit later, not instantly
	//
	deploymentArn, err := func(cluster, service *string, createdAt *time.Time, serviceConfigurationId string) (string, error) {
		for i := 1; i <= 12; i++ {
			ctx.Info("Polling deployments...")
			out, err := svc.ListServiceDeployments(context.TODO(), &ecs.ListServiceDeploymentsInput{
				Cluster: cluster,
				Service: service,
				CreatedAt: &types.CreatedAt{
					After: aws.Time(createdAt.Add(time.Duration(-1) * time.Minute)),
					// Before: aws.Time(time.Now()),
				},
			})
			if err != nil {
				return "", err
			}
			for _, deployment := range out.ServiceDeployments {
				// split target revision arn by '/' to take just its id
				bits := strings.Split(aws.ToString(deployment.TargetServiceRevisionArn), "/")
				// compare the revision we have with the one deploying
				deploymentId := bits[len(bits)-1] // last element
				if strings.Split(serviceConfigurationId, "/")[1] == deploymentId {
					arn := aws.ToString(deployment.ServiceDeploymentArn)
					ctx.Infof("Found deployment ARN '%s'", arn)
					return arn, nil
				}
			}
			time.Sleep(5 * time.Second)
		}

		return "", fmt.Errorf("timeout when retrieving deployment with service configuration %s", serviceConfigurationId)
	}(
		updateServiceOutput.Service.ClusterArn,
		updateServiceOutput.Service.ServiceArn,
		serviceConfigurations[0].CreatedAt,
		*serviceConfigurations[0].Id,
	)
	if err != nil {
		ctx.WithError(err).Error("Can't find deployment")
		return err
	}

	err = func(deploymentArn string) error {
		for i := 1; i <= 60; i++ {
			out, err := svc.DescribeServiceDeployments(context.TODO(), &ecs.DescribeServiceDeploymentsInput{
				ServiceDeploymentArns: []string{deploymentArn},
			})
			if err != nil {
				return err
			}
			status := out.ServiceDeployments[0].Status

			if status == types.ServiceDeploymentStatusSuccessful {
				// all good
				ctx.Infof("Deployment '%s' is complete", deploymentArn)
				return nil
			}

			if !(status == types.ServiceDeploymentStatusPending || status == types.ServiceDeploymentStatusInProgress) {
				return fmt.Errorf("Deployment is in '%s' status (%s)", status, aws.ToString(out.ServiceDeployments[0].StatusReason))
			}
			if i%3 == 0 { // print every 3rd
				ctx.Infof("Deployment is in %s", status)
			}
			time.Sleep(10 * time.Second)
		}
		return fmt.Errorf("timeout waiting for the deployment to complete")
	}(
		deploymentArn,
	)
	return err
}
