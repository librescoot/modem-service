BINARY_NAME := modem-service
BUILD_DIR := bin
GIT_REV := $(shell git describe --tags --always 2>/dev/null)
ifdef GIT_REV
LDFLAGS := -X main.version=$(GIT_REV)
else
LDFLAGS :=
endif
BUILDFLAGS := -tags netgo,osusergo
MAIN := ./cmd/modem-service

.PHONY: build build-host build-arm dist clean lint test fmt deps

build:
	mkdir -p $(BUILD_DIR)
	GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS) -s -w" $(BUILDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) $(MAIN)

build-arm: build

build-host:
	mkdir -p $(BUILD_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME) $(MAIN)

dist: build

clean:
	rm -rf $(BUILD_DIR)

lint:
	golangci-lint run

test:
	go test -v ./...

fmt:
	go fmt ./...

deps:
	go mod download && go mod tidy
