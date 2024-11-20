package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/gorilla/mux"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

type RepoRequest struct {
	RepoURL string `json:"repoURL"`
}

func fetchCodeFromGitHub(repoURL string, projectDir string) error {
	// Clone the GitHub repository into the dynamically created directory
	cmd := exec.Command("git", "clone", repoURL, projectDir)
	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("failed to clone repo: %v", err)
	}
	return nil
}

func getRepoNameFromURL(repoURL string) string {
	// Trim the "https://github.com/" prefix
	repoURL = strings.TrimPrefix(repoURL, "https://github.com/")

	// Split the string by "/"
	parts := strings.Split(repoURL, "/")

	// The repository name is the last part of the split string
	return parts[len(parts)-1]
}
func createProjectDir(repoName string) string {
	// Generate a directory name using timestamp and repo name
	timestamp := time.Now().Unix()
	projectDir := fmt.Sprintf("%s-%d", repoName, timestamp)

	// Create the directory if it doesn't exist
	err := os.MkdirAll(projectDir, os.ModePerm)
	if err != nil {
		fmt.Printf("Error creating project directory: %v\n", err)
		return ""
	}

	return projectDir
}

func deployHandler(w http.ResponseWriter, r *http.Request) {
	// Parse the JSON body to get the repo URL
	var req RepoRequest
	jsonErr := json.NewDecoder(r.Body).Decode(&req)

	if jsonErr != nil {
		http.Error(w, fmt.Sprintf("Error parsing request body: %v", jsonErr), http.StatusBadRequest)
		return
	}
	fmt.Println(req.RepoURL)

	// Extract the repository name dynamically
	repoName := getRepoNameFromURL(req.RepoURL)

	if repoName == "" {
		fmt.Println("Failed to extract repository name from URL.")
		return
	}
	fmt.Println("Extracted repository name:", repoName)
	// Create a dynamic directory for the project
	projectDir := createProjectDir(repoName)
	if projectDir == "" {
		fmt.Println("Failed to create project directory.")
		return
	}
	fmt.Println("Project directory created:", projectDir)

	// Clone the repository into the dynamically created directory
	err := fetchCodeFromGitHub(req.RepoURL, projectDir)
	if err != nil {
		fmt.Printf("Error fetching code: %v\n", err)
		return
	}

	buildTool := detectBuildTool(projectDir)

	if buildTool == "unknown" {
		fmt.Println("Unsupported build tool!")
		http.Error(w, fmt.Sprintf("Unsupported build tool! %v"), http.StatusBadRequest)
		return
	}

	// Execute the build inside a Docker container
	fetchErr := executeBuild(projectDir, buildTool)

	if fetchErr != nil {
		fmt.Printf("Build failed: %v\n", err)
		return
	}

	fmt.Println("Build completed successfully!")
}

func executeBuild(projectPath string, buildTool string) error {
	// Initialize Docker client
	cli, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		return fmt.Errorf("failed to initialize Docker client: %v", err)
	}
	// Generate Dockerfile
	err = generateDockerfile(buildTool)
	if err != nil {
		return err
	}
	// Build the Docker image
	buildContext, err := os.Open(projectPath)
	if err != nil {
		return fmt.Errorf("failed to open project directory: %v", err)
	}
	defer buildContext.Close()
	buildResp, err := cli.ImageBuild(context.Background(), buildContext, types.ImageBuildOptions{
		Dockerfile: "Dockerfile",
		Tags:       []string{"java-build-env"},
	})

	if err != nil {
		return fmt.Errorf("failed to build Docker image: %v", err)
	}
	defer buildResp.Body.Close()

	// Create container configuration
	containerConfig := &container.Config{
		Image: "java-build-env", // Use the built image
		Cmd:   []string{"/bin/sh", "-c", fmt.Sprintf("%s %s", buildTool, buildToolArgs(buildTool))},
	}

	// Host configuration (optional, can be set to nil for default)
	hostConfig := &container.HostConfig{}

	// Networking configuration (initialize as an empty struct)
	networkingConfig := &network.NetworkingConfig{} // Empty networking config

	// Platform configuration (optional, can be set to nil for default)
	platform := &v1.Platform{} // You can specify a platform if needed, or leave it as default

	// Create the container
	containerResp, err := cli.ContainerCreate(context.Background(), containerConfig, hostConfig, networkingConfig, platform, "")
	if err != nil {
		return fmt.Errorf("failed to create container: %v", err)
	}

	// Start the container
	err = cli.ContainerStart(context.Background(), containerResp.ID, container.StartOptions{})
	if err != nil {
		return fmt.Errorf("failed to start container: %v", err)
	}

	// Wait for the container to finish
	statusCh, errCh := cli.ContainerWait(context.Background(), containerResp.ID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("error while waiting for container: %v", err)
		}
	case status := <-statusCh:
		if status.StatusCode != 0 {
			return fmt.Errorf("container exited with non-zero status: %v", status.StatusCode)
		}
	}

	// Extract build artifacts (assuming JAR is at /app/target)
	copyResp, _, err := cli.CopyFromContainer(context.Background(), containerResp.ID, "/app/target")
	if err != nil {
		return fmt.Errorf("failed to copy build artifacts: %v", err)
	}
	defer copyResp.Close()

	// Handle copying to host (e.g., extracting the artifact to /build-output)
	err = os.MkdirAll("build-output", os.ModePerm)
	if err != nil {
		return fmt.Errorf("failed to create build output directory: %v", err)
	}

	// Copy file from container to host
	// You can use copy libraries or implement custom extraction logic
	return nil

}

func buildToolArgs(buildTool string) string {
	if buildTool == "maven" {
		return "clean package"
	}
	return "build"
}

func generateDockerfile(buildTool string) error {
	dockerfileContent := `
	FROM openjdk:17-jdk-slim
	RUN apt-get update && apt-get install -y curl git`

	// Add Maven or Gradle installation based on the build tool
	if buildTool == "maven" {
		dockerfileContent += "\nRUN apt-get install -y maven"
	} else if buildTool == "gradle" {
		dockerfileContent += "\nRUN apt-get install -y gradle"
	}
	// Set working directory in container
	dockerfileContent += `
	WORKDIR /app
	COPY . .`

	// Write Dockerfile to disk
	err := os.WriteFile("Dockerfile", []byte(dockerfileContent), 0644)
	if err != nil {
		return fmt.Errorf("failed to write Dockerfile: %v", err)
	}

	return nil
}
func detectBuildTool(projectPath string) string {
	// Check if pom.xml exists
	pomPath := filepath.Join(projectPath, "pom.xml")
	if _, err := os.Stat(pomPath); err == nil {
		return "maven"
	} else if os.IsNotExist(err) {
		fmt.Println("pom.xml does not exist at", pomPath)
	} else {
		fmt.Println("Error checking pom.xml:", err)
	}

	// Check if build.gradle exists
	gradlePath := filepath.Join(projectPath, "build.gradle")
	if _, err := os.Stat(gradlePath); err == nil {
		return "gradle"
	} else if os.IsNotExist(err) {
		fmt.Println("build.gradle does not exist at", gradlePath)
	} else {
		fmt.Println("Error checking build.gradle:", err)
	}

	// Return unknown if neither file is found
	return "unknown"
}

func main() {
	r := mux.NewRouter()

	r.HandleFunc("/deploy/{repoURL}", deployHandler).Methods("POST").Schemes("http")

	fmt.Println("Starting server on port 8080...")
	log.Fatal(http.ListenAndServe(":8080", r))
}
