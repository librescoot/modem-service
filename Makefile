BINARY_NAME := modem-service
GIT_REV := $(shell git describe --tags --always 2>/dev/null)
ifdef GIT_REV
LDFLAGS := -X main.version=$(GIT_REV)
else
LDFLAGS :=
endif
BUILDFLAGS := -tags netgo,osusergo
MAIN := ./cmd/modem-service

.PHONY: build build-host build-amd64 amd64 arm build-arm clean lint test fmt deps

dev: build
build:
	go build -ldflags "$(LDFLAGS)" -o ${BINARY_NAME} ${MAIN}

build-host:
	go build -ldflags "$(LDFLAGS)" -o ${BINARY_NAME} ${MAIN}

amd64:
	GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" $(BUILDFLAGS) -o ${BINARY_NAME}-amd64 ${MAIN}

build-amd64: amd64

arm:
	GOOS=linux GOARCH=arm GOARM=7 go build -ldflags "$(LDFLAGS)" $(BUILDFLAGS) -o ${BINARY_NAME}-arm ${MAIN}

build-arm: arm

dist:
	GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS) -s -w" $(BUILDFLAGS) -o ${BINARY_NAME}-arm-dist ${MAIN}

arm-debug:
	GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -gcflags="all=-N -l" $(BUILDFLAGS) -o ${BINARY_NAME}-arm-debug ${MAIN}

clean:
	rm -f ${BINARY_NAME} ${BINARY_NAME}-amd64 ${BINARY_NAME}-arm ${BINARY_NAME}-arm-dist ${BINARY_NAME}-arm-debug

lint:
	golangci-lint run

test:
	go test -v ./...

fmt:
	go fmt ./...

deps:
	go mod download && go mod tidy
