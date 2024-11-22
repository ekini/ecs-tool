package lib

import (
	"context"
	"fmt"
	"time"

	"github.com/apex/log"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecsTypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

// RunTask runs the specified one-off task in the cluster using the task definition
func RunTask(profile, cluster, service, taskDefinitionName, imageTag string, imageTags []string, workDir, containerName, awslogGroup string, launchType string, args []string) (exitCode int, err error) {
	err = makeConfig(profile)
	if err != nil {
		return 1, err
	}
	ctx := log.WithFields(&log.Fields{"task_definition": taskDefinitionName})

	svc := ecs.NewFromConfig(cfg)

	describeResult, err := svc.DescribeTaskDefinition(context.TODO(), &ecs.DescribeTaskDefinitionInput{
		TaskDefinition: aws.String(taskDefinitionName),
	})
	if err != nil {
		ctx.WithError(err).Error("Can't get task definition")
		return 1, err
	}
	taskDefinition := describeResult.TaskDefinition

	var foundContainerName bool
	if err := modifyContainerDefinitionImages(imageTag, imageTags, workDir, taskDefinition.ContainerDefinitions, ctx); err != nil {
		return 1, err
	}
	for n, containerDefinition := range taskDefinition.ContainerDefinitions {
		if aws.ToString(containerDefinition.Name) == containerName {
			foundContainerName = true
			taskDefinition.ContainerDefinitions[n].Command = args
			if awslogGroup != "" {
				// modify log output driver to capture output to a predefined CloudWatch log
				taskDefinition.ContainerDefinitions[n].LogConfiguration = &ecsTypes.LogConfiguration{
					LogDriver: ecsTypes.LogDriverAwslogs,
					Options: map[string]string{
						"awslogs-region":        cfg.Region,
						"awslogs-group":         awslogGroup,
						"awslogs-stream-prefix": cluster,
					},
				}
			}
		}
	}
	if !foundContainerName {
		err := fmt.Errorf("Can't find container with specified name in the task definition")
		ctx.WithFields(log.Fields{"container_name": containerName}).Error(err.Error())
		return 1, err
	}
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
		return 1, err
	}
	ctx.WithField(
		"task_definition_arn",
		aws.ToString(registerResult.TaskDefinition.TaskDefinitionArn),
	).Debug("Registered the task definition")

	// deregister the task definition
	defer func() {
		ctx = ctx.WithFields(log.Fields{"task_definition_arn": aws.ToString(registerResult.TaskDefinition.TaskDefinitionArn)})
		ctx.Debug("Deregistered the task definition")
		_, err = svc.DeregisterTaskDefinition(context.TODO(), &ecs.DeregisterTaskDefinitionInput{
			TaskDefinition: registerResult.TaskDefinition.TaskDefinitionArn,
		})
		if err != nil {
			ctx.WithError(err).Error("Can't deregister task definition")
		}
	}()

	runTaskInput := ecs.RunTaskInput{
		Cluster:        aws.String(cluster),
		TaskDefinition: registerResult.TaskDefinition.TaskDefinitionArn,
		Count:          aws.Int32(1),
		StartedBy:      aws.String("go-deploy"),
		LaunchType:     ecsTypes.LaunchType(launchType),
	}

	if service != "" {
		services, err := svc.DescribeServices(context.TODO(), &ecs.DescribeServicesInput{
			Cluster:  aws.String(cluster),
			Services: []string{service},
		})
		if err != nil {
			ctx.WithError(err).Error("Can't get service")
			return 1, err
		}

		runTaskInput.NetworkConfiguration = services.Services[0].NetworkConfiguration
	}

	runResult, err := svc.RunTask(context.TODO(), &runTaskInput)
	if err != nil {
		ctx.WithError(err).Error("Can't run specified task")
		return 1, err
	}

	// if there are no running/pending tasks, then it failed to start
	if len(runResult.Tasks) == 0 {
		ctx.Error("No tasks could be run. Please check if the ECS cluster has enough resources")
		return 1, err
	}
	// the task should be in PENDING state at this point

	ctx.Info("Waiting for the task to finish")
	var tasks []string
	for _, task := range runResult.Tasks {
		tasks = append(tasks, aws.ToString(task.TaskArn))
		ctx.WithField("task_arn", aws.ToString(task.TaskArn)).Debug("Started task")
	}
	tasksInput := &ecs.DescribeTasksInput{
		Cluster: aws.String(cluster),
		Tasks:   tasks,
	}
	waiter := ecs.NewTasksStoppedWaiter(svc)
	err = waiter.Wait(context.TODO(), tasksInput, 10*time.Minute)
	if err != nil {
		ctx.WithError(err).Error("The waiter has been finished with an error")
		exitCode = 3
	}
	tasksOutput, err := svc.DescribeTasks(context.TODO(), tasksInput)
	if err != nil {
		ctx.WithError(err).Error("Can't describe stopped tasks")
		return 1, err
	}
	for _, task := range tasksOutput.Tasks {
		for _, container := range task.Containers {
			ctx := log.WithFields(log.Fields{
				"container_name": aws.ToString(container.Name),
			})
			reason := aws.ToString(container.Reason)
			if len(reason) != 0 {
				exitCode = 11
				ctx = ctx.WithField("reason", reason)
			} else {
				ctx = ctx.WithField("exit_code", aws.ToInt32(container.ExitCode))
			}
			if aws.ToInt32(container.ExitCode) == 0 && len(reason) == 0 {
				ctx.Info("Container exited")
			} else {
				ctx.Error("Container exited")
			}
			if aws.ToString(container.Name) == containerName {
				if len(reason) == 0 {
					exitCode = int(aws.ToInt32(container.ExitCode))
					if awslogGroup != "" {
						// get log output
						taskUUID, err := parseTaskUUID(aws.ToString(container.TaskArn))
						if err != nil {
							log.WithFields(log.Fields{"task_arn": aws.ToString(container.TaskArn)}).WithError(err).Error("Can't parse task uuid")
							exitCode = 10
							continue
						}
						err = fetchCloudWatchLog(cluster, containerName, awslogGroup, taskUUID, false, ctx)
						if err != nil {
							log.WithError(err).Error("Can't fetch the logs")
							exitCode = 10
						}
					}
				}
			}
		}
	}

	return
}
