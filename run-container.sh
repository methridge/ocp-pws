#!/bin/bash

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Default values
IMAGE_NAME="ocp-pws"
IMAGE_TAG="minimal"
CONTAINER_NAME="ocp-pws"
PORT="8080"
DETACHED=false
REMOVE_ON_EXIT=true

# Help function
show_help() {
    echo "Usage: $0 [OPTIONS]"
    echo ""
    echo "Run the OCP PWS container with environment variables from .envrc"
    echo ""
    echo "Options:"
    echo "  -i, --image IMAGE      Docker image name:tag (default: ocp-pws:minimal)"
    echo "  -n, --name NAME        Container name (default: ocp-pws)"
    echo "  -p, --port PORT        Host port to bind to (default: 8080)"
    echo "  -d, --detach           Run container in background"
    echo "  -r, --rm               Remove container when it exits"
    echo "  --stop                 Stop and remove existing container"
    echo "  --logs                 Show logs of running container"
    echo "  --shell                Open shell in running container (if possible)"
    echo "  -h, --help             Show this help message"
    echo ""
    echo "Examples:"
    echo "  $0                                    # Run with defaults"
    echo "  $0 -d -r                             # Run detached and remove on exit"
    echo "  $0 -i ocp-pws:latest -p 3000        # Custom image and port"
    echo "  $0 --stop                            # Stop existing container"
    echo "  $0 --logs                            # Show container logs"
    echo ""
}

# Function to load environment variables from .envrc
load_envrc() {
    if [[ -f ".envrc" ]]; then
        echo -e "${BLUE}Loading environment variables from .envrc...${NC}"

        # Check if direnv is available
        if command -v direnv &> /dev/null; then
            echo -e "${YELLOW}Using direnv to load .envrc${NC}"
            eval "$(direnv export bash)"
        else
            echo -e "${YELLOW}direnv not found, sourcing .envrc directly${NC}"
            # Source .envrc while handling 1Password CLI calls
            set -a  # Export all variables
            while IFS= read -r line; do
                # Skip comments and empty lines
                [[ "$line" =~ ^[[:space:]]*# ]] && continue
                [[ -z "${line// }" ]] && continue

                # Handle export statements
                if [[ "$line" =~ ^export[[:space:]]+ ]]; then
                    # Remove 'export ' prefix
                    line="${line#export }"

                    # Check if line contains 1Password CLI call
                    if [[ "$line" =~ \$\(op[[:space:]] ]]; then
                        echo -e "${YELLOW}Executing 1Password CLI command in: ${line}${NC}"
                        eval "$line"
                    else
                        # Simple variable assignment
                        eval "$line"
                    fi
                fi
            done < .envrc
            set +a  # Stop exporting
        fi

        # Verify required environment variables are set
        local required_vars=("API" "STATION_ID" "UNITS" "API_KEY" "RANDOM_SECRET")
        local missing_vars=()

        for var in "${required_vars[@]}"; do
            if [[ -z "${!var}" ]]; then
                missing_vars+=("$var")
            fi
        done

        if [[ ${#missing_vars[@]} -gt 0 ]]; then
            echo -e "${RED}Error: Missing required environment variables:${NC}"
            printf '%s\n' "${missing_vars[@]}"
            echo ""
            echo "Please ensure your .envrc file contains all required variables:"
            echo "  export API=\"https://api.weather.com/v2/pws/observations/current\""
            echo "  export STATION_ID=\"YOUR_STATION_ID\""
            echo "  export UNITS=\"e\""
            echo "  export API_KEY=\"YOUR_API_KEY\""
            echo "  export RANDOM_SECRET=\"YOUR_RANDOM_SECRET\""
            exit 1
        fi

        echo -e "${GREEN}Environment variables loaded successfully:${NC}"
        echo "  API: ${API}"
        echo "  STATION_ID: ${STATION_ID}"
        echo "  UNITS: ${UNITS}"
        echo "  API_KEY: [HIDDEN]"
        echo "  RANDOM_SECRET: [HIDDEN]"
        echo ""
    else
        echo -e "${RED}Error: .envrc file not found${NC}"
        echo "Please create a .envrc file with the required environment variables."
        exit 1
    fi
}

# Function to stop and remove existing container
stop_container() {
    if docker ps -a --format '{{.Names}}' | grep -q "^${CONTAINER_NAME}$"; then
        echo -e "${BLUE}Stopping and removing existing container: ${CONTAINER_NAME}${NC}"
        docker stop "$CONTAINER_NAME" >/dev/null 2>&1 || true
        docker rm "$CONTAINER_NAME" >/dev/null 2>&1 || true
    fi
}

# Function to show container logs
show_logs() {
    if docker ps --format '{{.Names}}' | grep -q "^${CONTAINER_NAME}$"; then
        echo -e "${BLUE}Showing logs for container: ${CONTAINER_NAME}${NC}"
        docker logs -f "$CONTAINER_NAME"
    else
        echo -e "${RED}Container ${CONTAINER_NAME} is not running${NC}"
        exit 1
    fi
}

# Function to open shell in container
open_shell() {
    if docker ps --format '{{.Names}}' | grep -q "^${CONTAINER_NAME}$"; then
        echo -e "${BLUE}Opening shell in container: ${CONTAINER_NAME}${NC}"
        echo -e "${YELLOW}Note: This container uses a minimal base image and may not have a shell${NC}"
        docker exec -it "$CONTAINER_NAME" /bin/sh 2>/dev/null || \
        docker exec -it "$CONTAINER_NAME" /bin/bash 2>/dev/null || \
        echo -e "${RED}No shell available in this minimal container${NC}"
    else
        echo -e "${RED}Container ${CONTAINER_NAME} is not running${NC}"
        exit 1
    fi
}

# Parse command line arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        -i|--image)
            if [[ "$2" == *":"* ]]; then
                IMAGE_NAME="${2%:*}"
                IMAGE_TAG="${2#*:}"
            else
                IMAGE_NAME="$2"
            fi
            shift 2
            ;;
        -n|--name)
            CONTAINER_NAME="$2"
            shift 2
            ;;
        -p|--port)
            PORT="$2"
            shift 2
            ;;
        -d|--detach)
            DETACHED=true
            shift
            ;;
        -r|--rm)
            REMOVE_ON_EXIT=true
            shift
            ;;
        --stop)
            stop_container
            exit 0
            ;;
        --logs)
            show_logs
            exit 0
            ;;
        --shell)
            open_shell
            exit 0
            ;;
        -h|--help)
            show_help
            exit 0
            ;;
        *)
            echo -e "${RED}Unknown option: $1${NC}"
            show_help
            exit 1
            ;;
    esac
done

# Check if docker is running
if ! docker info &> /dev/null; then
    echo -e "${RED}Docker is not running. Please start Docker first.${NC}"
    exit 1
fi

# Load environment variables
load_envrc

# Check if image exists
FULL_IMAGE="${IMAGE_NAME}:${IMAGE_TAG}"
if ! docker image inspect "$FULL_IMAGE" &> /dev/null; then
    echo -e "${RED}Image ${FULL_IMAGE} not found locally.${NC}"
    echo -e "${YELLOW}Would you like to build it? (y/N)${NC}"
    read -r response
    if [[ "$response" =~ ^[Yy]$ ]]; then
        if [[ -f "build-docker.sh" ]]; then
            echo -e "${BLUE}Building image...${NC}"
            ./build-docker.sh -n "$IMAGE_NAME" -t "$IMAGE_TAG"
        else
            echo -e "${RED}build-docker.sh not found. Please build the image manually.${NC}"
            exit 1
        fi
    else
        echo -e "${RED}Cannot run container without image. Exiting.${NC}"
        exit 1
    fi
fi

# Stop existing container if running
stop_container

# Build docker run command
DOCKER_CMD="docker run"

# Add flags
if [[ "$DETACHED" == true ]]; then
    DOCKER_CMD="$DOCKER_CMD -d"
else
    DOCKER_CMD="$DOCKER_CMD -it"
fi

if [[ "$REMOVE_ON_EXIT" == true ]]; then
    DOCKER_CMD="$DOCKER_CMD --rm"
fi

# Add container configuration
DOCKER_CMD="$DOCKER_CMD --platform linux/amd64"
DOCKER_CMD="$DOCKER_CMD --name $CONTAINER_NAME"
DOCKER_CMD="$DOCKER_CMD -p $PORT:8080"

# Add environment variables
DOCKER_CMD="$DOCKER_CMD -e API=\"$API\""
DOCKER_CMD="$DOCKER_CMD -e STATION_ID=\"$STATION_ID\""
DOCKER_CMD="$DOCKER_CMD -e UNITS=\"$UNITS\""
DOCKER_CMD="$DOCKER_CMD -e API_KEY=\"$API_KEY\""
DOCKER_CMD="$DOCKER_CMD -e RANDOM_SECRET=\"$RANDOM_SECRET\""

# Add debug flag if set
if [[ -n "$DEBUG" ]]; then
    DOCKER_CMD="$DOCKER_CMD -e DEBUG=\"$DEBUG\""
fi

# Add image
DOCKER_CMD="$DOCKER_CMD $FULL_IMAGE"

echo -e "${BLUE}Starting OCP PWS container...${NC}"
echo -e "${YELLOW}Image:${NC} $FULL_IMAGE"
echo -e "${YELLOW}Container:${NC} $CONTAINER_NAME"
echo -e "${YELLOW}Port:${NC} $PORT -> 8080"
echo -e "${YELLOW}Mode:${NC} $([ "$DETACHED" == true ] && echo "detached" || echo "interactive")"
echo ""

# Execute the command
eval "$DOCKER_CMD"

if [[ "$DETACHED" == true ]]; then
    echo ""
    echo -e "${GREEN}Container started successfully!${NC}"
    echo -e "${BLUE}Container ID:${NC} $(docker ps --filter name=$CONTAINER_NAME --format '{{.ID}}')"
    echo -e "${BLUE}Access URL:${NC} http://localhost:$PORT"
    echo ""
    echo -e "${YELLOW}Useful commands:${NC}"
    echo "  $0 --logs          # Show container logs"
    echo "  $0 --stop          # Stop and remove container"
    echo "  docker ps          # List running containers"
    echo ""
else
    echo -e "${YELLOW}Container stopped.${NC}"
fi
