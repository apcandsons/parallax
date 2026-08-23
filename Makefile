BINDIR ?= $(HOME)/.local/bin

build:
	go build -o bin/parallax .

install:
	mkdir -p $(BINDIR)
	go build -o $(BINDIR)/parallax .

test:
	go test ./...

vet:
	go vet ./...

.PHONY: build install test vet
