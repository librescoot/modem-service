BIN := rescoot-modem
GIT_REV := $(shell git describe --tags --always 2>/dev/null)
ifdef GIT_REV
LDFLAGS := -X main.version=$(GIT_REV)
else
LDFLAGS :=
endif
BUILDFLAGS := -tags netgo,osusergo
MAIN := ./cmd/modem-service

.PHONY: build amd64 arm clean

dev: build
build:
	go build -ldflags "$(LDFLAGS)" -o ${BIN} ${MAIN}

amd64:
	GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" $(BUILDFLAGS) -o ${BIN}-amd64 ${MAIN}

arm:
	GOOS=linux GOARCH=arm GOARM=7 go build -ldflags "$(LDFLAGS)" $(BUILDFLAGS) -o ${BIN}-arm ${MAIN}

dist:
	GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS) -s -w" $(BUILDFLAGS) -o ${BIN}-arm-dist ${MAIN}

arm-debug:
	GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -gcflags="all=-N -l" $(BUILDFLAGS) -o ${BIN}-arm-debug ${MAIN}

clean:
	rm -f ${BIN} ${BIN}-amd64 ${BIN}-arm ${BIN}-arm-dist ${BIN}-arm-debug
