package main

import (
	"log"
	"os"
	"strings"

	"dockerci/src/api"
	"dockerci/src/docker"

	"github.com/docker/docker/api/types/events"
	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
)

var client *docker.DockerClient
var enabledContainers []docker.ContainerInfo

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
	client = docker.New()
	client.Events[docker.Create_container] = onCreateContainer
	client.Events[docker.Destroy_container] = onDestroyContainer
	go client.ListenToEvents()
	loadContainersConfig()
	api.New(&enabledContainers, onRequest).Serve()
}

func loadContainersConfig() {
	containers := client.GetContainersEnabled()
	enabledContainers = make([]docker.ContainerInfo, len(containers))
	for _, container := range containers {
		name := container.Names[0][1:]
		enabledContainers = append(enabledContainers, docker.ContainerInfo{Names: container.Names, Id: container.ID})
		log.Printf("Webhook available at: %s/hooks/%s", os.Getenv("BASE_URL"), name)
	}
}
func onRequest(name string, token string, sock *websocket.Conn) (int, string) {
	containerInfos := getContainerFromName(name)
	if containerInfos == nil {
		return 400, "Container not found"
	}
	log.Println("Request received for service:", name)
	if err := client.NewRequest(containerInfos.Id, name, token, sock); err != nil {
		log.Println("Error updating container "+name, err.Error())
		return 500, "Failed to update container " + name
	}
	log.Printf("Container %s successfully updated", name)
	return 200, "Done"
}
func onCreateContainer(msg events.Message) {
	if client.IsContainerEnabled(msg.Actor.ID) {
		log.Println("Container creation detected:", msg.Actor.Attributes["name"])
		defer loadContainersConfig()
	}
}
func onDestroyContainer(msg events.Message) {
	defer loadContainersConfig()
}

//Get a ContainerInfo object from a container name
func getContainerFromName(name string) *docker.ContainerInfo {
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
