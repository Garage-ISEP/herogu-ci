package main

import (
	"log"
	"os"
	"strings"

	"github.com/docker/docker/api/types/events"
	"github.com/joho/godotenv"
)

type ContainerInfo struct {
	Names []string
	Id    string
}

var docker *DockerClient
var enabledContainers []ContainerInfo

//Parse the environment variables
//Init docker instance and bind events
//Start event listening and load current container config
//Start the http server
func main() {
	if os.Getenv("DOCKER_HOST") == "" {
		err := godotenv.Load()
		if err != nil {
			log.Fatal("Error loading .env file")
		}
	}
	docker = InitDockerInstance()
	docker.events[create_container] = onCreateContainer
	docker.events[destroy_container] = onDestroyContainer
	go docker.ListenToEvents()
	loadContainersConfig()
	startServer(onRequest)
}

func loadContainersConfig() {
	containers := docker.GetContainersEnabled()
	enabledContainers = make([]ContainerInfo, len(containers))
	for _, container := range containers {
		name := container.Names[0][1:]
		enabledContainers = append(enabledContainers, ContainerInfo{container.Names, container.ID})
		log.Printf("Webhook available at: %s/hooks/%s", os.Getenv("BASE_URL"), name)
	}
}
func onRequest(name string) (int, string) {
	containerInfos := getContainerFromName(name)
	if containerInfos == nil {
		return 400, "Container not found"
	}
	log.Println("Request received for service:", name)
	if err := docker.UpdateContainer(containerInfos.Id); err != nil {
		return 500, "Failed to update container " + name
	}
	log.Printf("Container %s successfully updated", name)
	return 200, "Done"
}
func onCreateContainer(msg events.Message) {
	log.Println("Container creation detected:", msg.Actor.Attributes["name"])
	defer loadContainersConfig()
}
func onDestroyContainer(msg events.Message) {
	log.Println("Container deletion detected:", msg.Actor.Attributes["name"])
	defer loadContainersConfig()
}

//Get a ContainerInfo object from a container name
func getContainerFromName(name string) *ContainerInfo {
	name = strings.ToLower(name)
	for _, container := range enabledContainers {
		for _, containerName := range container.Names {
			if strings.ToLower(containerName) == "/"+name {
				return &container
			}
		}
	}
	return nil
}