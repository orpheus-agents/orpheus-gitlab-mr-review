-include .env

ROOT_DIR=$(shell dirname $(realpath $(lastword $(MAKEFILE_LIST))))
CONFIG_FILE=$(ROOT_DIR)/config.yml
SHELL=/bin/bash -euo pipefail

.env:
	@cp .env.dist .env

up: .env
	@docker compose up -d

build: .env
	@echo "==> Building"
	@docker compose run --rm \
		-e GOOS=linux \
		-e CGO_ENABLED=0 \
	golang go build -buildvcs=false -trimpath -o bin/app .
	@docker compose run --rm golang chmod +x bin/app

vet:
	@echo "==> Running go vet"
	@docker compose run --rm golang go vet -composites=false ./...

lint:
	@docker run --rm -v $(ROOT_DIR):/var/app -w /var/app golangci/golangci-lint:v2.15.0 golangci-lint run ./...

test: up migrate
	@echo "==> Running go test"
	@docker compose run --rm golang go test -coverprofile test.cov -v -short -cover -race ./...
	@docker compose run --rm golang go tool cover -func test.cov

tidy:
	@docker compose run --rm golang go mod tidy
