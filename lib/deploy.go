package lib

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/apex/log"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/samber/lo"
)

// DeployServices deploys specified services in parallel
func DeployServices(profile, cluster, imageTag string, imageTags, services []string, workDir string, rollbackOnFail bool) (exitCode int, err error) {
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
			deployService(ctx, cluster, imageTag, imageTags, workDir, service, exits, rollback, &wg)
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

func deployService(ctx log.Interface, cluster, imageTag string, imageTags []string, workDir, service string, exitChan chan int, rollback chan bool, wg *sync.WaitGroup) {
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

func updateService(ctx log.Interface, cluster, service, taskDefinition string) error {
	svc := ecs.NewFromConfig(cfg)
	// update the service using the new registered task definition
	_, err := svc.UpdateService(context.TODO(), &ecs.UpdateServiceInput{
		Cluster:        aws.String(cluster),
		Service:        aws.String(service),
		TaskDefinition: aws.String(taskDefinition),
	})
	if err != nil {
		ctx.WithError(err).Error("Can't update the service")
		return err
	}
	ctx.Info("Updated the service")
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

	ctx.Info("Service has been deployed")
	return nil
}
