
.GIT_COMMIT=$(shell git rev-parse HEAD)
.GIT_VERSION=$(shell git describe --tags --always --dirty 2>/dev/null)
.GIT_UNTRACKEDCHANGES := $(shell git status --porcelain --untracked-files=no)
ifneq ($(.GIT_UNTRACKEDCHANGES),)
	.GIT_VERSION := $(.GIT_VERSION)-$(shell date +"%s")
endif
LDFLAGS := "-s -w -X main.Version=$(.GIT_VERSION) -X main.GitCommit=$(.GIT_COMMIT)"

SERVER?=10.119.46.41:30003/cc
OWNER?=openfaas
IMG_NAME?=of-watchdog
TAG?=latest
IMAGE=$(SERVER)/$(OWNER)/$(IMG_NAME)
export GOFLAGS=-mod=vendor

.PHONY: all
all: gofmt test dist hashgen

.PHONY: test
test:
	@echo "+ $@"
	@go test -v ./...

.PHONY: gofmt
gofmt:
	@echo "+ $@"
	@gofmt -l -d $(shell find . -type f -name '*.go' -not -path "./vendor/*")


.PHONY: build
build:
	@echo "+ $@"
	@docker build \
		--build-arg GIT_COMMIT=${.GIT_COMMIT} \
		--build-arg VERSION=${.GIT_VERSION} \
		-t ${IMAGE}:${TAG} . &&\
	docker push ${IMAGE}:${TAG}

.PHONY: hashgen
hashgen:
	./ci/hashgen.sh

.PHONY: dist
dist:
	@echo "+ $@"
	CGO_ENABLED=0 GOOS=linux go build -mod=vendor -a -ldflags $(LDFLAGS) -installsuffix cgo -o bin/fwatchdog-amd64
# use this with
# `./ci/copy_redist.sh $(make print-image) && ./ci/hashgen.sh`
print-image:
	@echo ${.IMAGE}

# Example: 
# SERVER=docker.io OWNER=alexellis2 TAG=ready make publish
.PHONY: publish
publish:
	@echo  $(SERVER)/$(OWNER)/$(IMG_NAME):$(TAG) && \
	docker buildx create --use --name=multiarch --node=multiarch && \
	docker buildx build \
		--platform linux/amd64\
		--push=true \
        --build-arg GIT_COMMIT=$(GIT_COMMIT) \
        --build-arg VERSION=$(VERSION) \
		--tag $(SERVER)/$(OWNER)/$(IMG_NAME):$(TAG) \
		.