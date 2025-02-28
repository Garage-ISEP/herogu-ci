package docker

import (
	"bufio"
	"context"
	"dockerci/src/utils"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/ioutil"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/gorilla/websocket"
)

//Represent the container update process
type ContainerAgent struct {
	docker         *DockerClient
	cli            *client.Client
	containerId    string
	name           string
	token          string
	containerInfos types.ContainerJSON
	imageInfos     types.ImageInspect
	ctx            context.Context
	sock           *websocket.Conn
}

func NewContainerAgent(docker *DockerClient, containerId string, name string, token string, sock *websocket.Conn) *ContainerAgent {
	ctx := context.Background()
	containerInfos, err := docker.cli.ContainerInspect(ctx, containerId)
	imageInfos, _, err1 := docker.cli.ImageInspectWithRaw(ctx, containerInfos.Image)
	if err != nil || err1 != nil {
		log.Println("Error while fetching container infos")
		return nil
	}
	return &ContainerAgent{
		docker:         docker,
		containerId:    containerId,
		containerInfos: containerInfos,
		imageInfos:     imageInfos,
		name:           name,
		ctx:            ctx,
		cli:            docker.cli,
		sock:           sock,
		token:          token,
	}
}

//This method will pull the container image, check if it is the same that the current
//In case of a new one the container will be recreated and restarted
//If the image has to be buit from a git repo it will build the image locally
func (agent *ContainerAgent) UpdateContainer() (err error) {
	defer func() {
		if r := recover(); r != nil {
			switch t := r.(type) {
			case string:
				err = errors.New(t)
				agent.emit(Error, t)
			case error:
				err = t
				agent.emit(Error, err.Error())
			default:
				err = errors.New("unknown panic")
			}
			agent.emit(Error, map[string]interface{}{"error": err.Error()})
		}
	}()
	if err != nil {
		agent.panic("Error while fetching container", err)
	}
	agent.emit(Start, nil)
	if agent.isLocalImage() {
		agent.print("Container is local image")
		agent.emit(Build, nil)
		context := agent.getLabel("context")
		if context == "" {
			context = "."
		}
		dockerfile := agent.getLabel("dockerfile")
		if dockerfile == "" {
			dockerfile = "Dockerfile"
		}
		repo := agent.getLabel("repo")
		status, err := agent.buildDockerImage(repo, dockerfile, agent.containerInfos.Config.Image, agent.getImageLabel("repo-sha"))
		if err != nil {
			agent.panic("Error while building image", err)
		}
		agent.emit(BuildEnd, map[string]interface{}{"status": status})
		if !status {
			return nil
		}
	} else {
		agent.print("Container is external image")
		agent.emit(Pull, nil)
		//Pulling Image
		authToken := agent.getContainerCredsToken()
		agent.print(agent.containerInfos.Config.Image)
		status, err := agent.pullImage(agent.containerInfos.Config.Image, authToken, agent.imageInfos)
		if err != nil {
			agent.panic(err)
		}
		agent.emit(PullEnd, map[string]interface{}{"status": status})
		if !status {
			return nil
		}
	}

	//Stopping Container
	agent.emit(Stop, nil)
	if agent.containerInfos.State.Running {
		duration, _ := time.ParseDuration("5s")
		if err = agent.cli.ContainerStop(agent.ctx, agent.containerId, &duration); err != nil {
			agent.panic("Error while stopping container:", err)
		}
	}
	//Removing Container
	agent.emit(Remove, nil)
	agent.cli.ContainerRemove(agent.ctx, agent.containerId, types.ContainerRemoveOptions{
		RemoveVolumes: false, RemoveLinks: false, Force: true,
	})
	//Recreating Container
	agent.emit(Recreate, nil)
	createdContainer, err := agent.cli.ContainerCreate(agent.ctx, agent.containerInfos.Config, agent.containerInfos.HostConfig, nil, nil, agent.containerInfos.Name)
	if err != nil {
		agent.panic("Error while creating container:", err)
	}
	//Starting Container
	agent.emit(Start, nil)
	if err := agent.cli.ContainerStart(agent.ctx, createdContainer.ID, types.ContainerStartOptions{}); err != nil {
		agent.panic("Error while starting container:", err)
	}
	//Removing former image
	agent.emit(RemoveImage, nil)
	if _, err := agent.cli.ImageRemove(agent.ctx, agent.imageInfos.ID, types.ImageRemoveOptions{Force: true}); err != nil {
		agent.panic("Error while removing former image:", err)
	}
	filterArgs := filters.NewArgs(filters.KeyValuePair{Key: "dangling", Value: "true"})
	//Remove all untagged image
	if _, err = agent.cli.ImagesPrune(agent.ctx, filterArgs); err != nil {
		agent.panic("Error while removing untagged image:", err)
	}
	agent.emit(End, nil)
	return err
}

//Building Image from git repository
func (agent *ContainerAgent) buildDockerImage(repoLink string, dockerfile string, image string, previousSha string) (status bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = errors.New(r.(string))
		}
	}()
	//We replace the {{TOKEN}} by the token
	reg, err := regexp.Compile(`{{.+}}`)
	if err != nil {
		agent.panic("Error while compiling regexp:", err.Error())
	}
	remoteLink := reg.ReplaceAllString(repoLink, agent.token)
	lastCommitSha, err := agent.getLastCommitSha(remoteLink)
	if err != nil {
		agent.panic("Error while getting last commit sha: ", err)
	}
	if previousSha == lastCommitSha {
		agent.print("Image already up to date, stopping process...")
		return false, nil
	}
	reader, err := agent.cli.ImageBuild(agent.ctx, nil, types.ImageBuildOptions{
		RemoteContext: remoteLink,
		Dockerfile:    dockerfile,
		NoCache:       true,
		ForceRemove:   true,
		Remove:        true,
		Tags:          []string{image},
		Labels:        map[string]string{"docker-ci.repo-sha": lastCommitSha},
	})
	if err != nil {
		agent.panic("while building image:", err)
	}
	scanner := bufio.NewScanner(reader.Body)
	for scanner.Scan() {
		line := scanner.Text()
		agent.emit(BuildMessage, line)
	}
	defer reader.Body.Close()
	return true, err
}

//Pull an image from a container registry with optional credentials
//If the image already exists it returns false and
//If the image is successfuly pulled it returns true
func (agent *ContainerAgent) pullImage(image string, authToken string, imageInfos types.ImageInspect) (status bool, err error) {
	reader, err := agent.cli.ImagePull(agent.ctx, image, types.ImagePullOptions{All: false, RegistryAuth: authToken})
	if err != nil {
		return false, errors.New("Error while pulling image:" + err.Error())
	}
	scanner := bufio.NewScanner(reader)
	regex, err := regexp.Compile(`\b(sha256:[A-Fa-f0-9]{64})\b`)
	if err != nil {
		return false, errors.New("Error while compiling regex: " + err.Error())
	}
	//While pulling image we check if the image is new
	//If not we stop the update process
	for scanner.Scan() {
		line := scanner.Text()
		agent.emit(PullMessage, line)
		if sha := regex.FindString(line); sha != "" {
			agent.print("Pulling image with digest:", sha)
			for _, digest := range imageInfos.RepoDigests {
				//We get the digest from the repo digest (name@digest)
				if regex.FindString(digest) == sha {
					agent.print("Image already up to date, stopping process...")
					return false, nil
				}
			}
		}
	}
	defer reader.Close()
	return true, err
}

//Panic with container name
func (agent *ContainerAgent) panic(args ...interface{}) {
	log.Panicf("[%s] %v", agent.name, strings.Join(utils.InterfaceToStringSlice(args), " "))
}

//Print with container name
func (agent *ContainerAgent) print(args ...interface{}) {
	log.Printf("[%s] %v", agent.name, strings.Join(utils.InterfaceToStringSlice(args), " "))
}

//Determine if the container image is local or external from the label
//If it contains a repo label it means that it is built locally from repository
func (agent *ContainerAgent) isLocalImage() bool {
	return agent.getLabel("repo") != ""
}

//Read auth config from container labels and return a base64 encoded string for docker.
func (agent *ContainerAgent) getContainerCredsToken() string {
	serveraddress := agent.getLabel("auth-server")
	password := agent.getLabel("password")
	username := agent.getLabel("username")
	if serveraddress != "" && username != "" && password != "" {
		data, err := json.Marshal(DockerAuth{Username: username, Password: password, Serveraddress: serveraddress})
		if err != nil {
			agent.panic("Error while marshalling auth config:", err)
		}
		auth := base64.StdEncoding.EncodeToString(data)
		return string(auth)
	} else {
		return ""
	}
}

//Get the last commit sha from the git repository using git protocol
//Regexs : https://regexr.com/6b5f6,
func (agent *ContainerAgent) getLastCommitSha(remote string) (string, error) {
	//We find the from the remote url branch name
	branch := strings.Replace(regexp.MustCompile(`#[A-Za-z]+`).FindString(remote), "#", "", -1)
	if branch == "" {
		branch = "master"
	}
	//We get the last commit sha from the git protocol on the selected branch
	remoteUrl := regexp.MustCompile(`(#\S+)|\.git`).ReplaceAllString(remote, "")
	req, _ := http.NewRequest("GET", remoteUrl, nil)
	req.Header.Set("User-Agent", "Docker-CI")
	req.Header.Set("Content-Type", "application/x-git-upload-pack")
	resp, err := http.Get(remoteUrl + ".git/info/refs?service=git-upload-pack")
	if err != nil {
		return "", err
	}
	body, err := ioutil.ReadAll(resp.Body)
	sha := strings.Split(regexp.MustCompile(`[0-9a-f]{5,50} refs/heads/`+branch).FindString(string(body)), " ")[0]
	sha = sha[len(sha)-40:]
	if err != nil {
		return "", err
	}
	return sha, nil
}

//Emit a message to the current socket
func (agent *ContainerAgent) emit(event StreamEvent, data interface{}) {
	var dataStruct []byte
	if agent.sock != nil {
		switch t := data.(type) {
		case string:
			dataStruct = []byte(t)
		case nil:
			break
		case error:
			dataStruct = []byte(t.Error())
		default:
			dataStruct = utils.ToJSON(t)
		}
		agent.sock.WriteMessage(websocket.TextMessage, dataStruct)
	}
}

//Get a docker-ci container label value
func (agent *ContainerAgent) getLabel(key string) string {
	return agent.containerInfos.Config.Labels["docker-ci."+key]
}

//Get a docker-ci image label value
func (agent *ContainerAgent) getImageLabel(key string) string {
	return agent.imageInfos.Config.Labels["docker-ci."+key]
}
