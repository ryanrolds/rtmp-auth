# parameters
GOBUILD=CGO_ENABLED=0 go build
GOCLEAN=go clean
PROTOC=protoc
STATIK=statik
BINARY_NAME=rtmp-auth

PROTO_GENERATED=storage/storage.pb.go
STATIK_GENERATED=statik/statik.go
PUBLIC_FILES=$(wildcard public/*)

.DEFAULT_GOAL := build

%.pb.go: %.proto
	$(PROTOC) -I=storage/ --go_opt=paths=source_relative --go_out=storage/ $<

$(STATIK_GENERATED): $(PUBLIC_FILES)
	echo "$(PUBLIC_FILES)"
	$(STATIK) -f -src=public/ -dest=.

reqs-debian:
	sudo apt update
	sudo apt install -y protobuf-compiler

	export GOPATH="$(HOME)/go"
	export PATH=$(PATH):$(GOPATH)/bin"

	go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.28
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.2
	go install github.com/rakyll/statik

.PHONY: reqs

build: $(PROTO_GENERATED) $(STATIK_GENERATED)
	export GOPATH="$(HOME)/go"
	export PATH=$(PATH):$(GOPATH)/bin"

	$(GOBUILD) -o $(BINARY_NAME) -v ./cmd/rtmp-auth
.PHONY: build

clean:
	$(GOCLEAN)
	rm -f $(PROTO_GENERATED)
	rm -f $(STATIK_GENERATED)
.PHONY: clean

all: build
.PHONY: all
