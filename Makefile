BINARY := floorline
PKG    := ./cmd/floorline

.PHONY: help build test lint run smoke backfill clean image image-test up down logs

help:
	@echo "make build     compile ./$(BINARY)"
	@echo "make test      go vet + go test ./..."
	@echo "make smoke     probe every Tonnel endpoint (needs a live .env)"
	@echo "make backfill  download trade history"
	@echo "make run       start the pollers and the bot"
	@echo "make clean     remove the binary"
	@echo "make image     build the production container (tests run inside)"
	@echo "make up        build and start via docker compose"
	@echo "make down      stop the stack, keeping the data volume"
	@echo "make logs      follow the container log"

build:
	go build -o $(BINARY) $(PKG)

test:
	go vet ./...
	go test ./...

lint:
	gofmt -l .

run: build
	./$(BINARY) run

smoke: build
	./$(BINARY) smoke

backfill: build
	./$(BINARY) backfill

clean:
	rm -f $(BINARY)

# The image runs the suite in its own stage, so a red build cannot be deployed.
image: image-test
	DOCKER_BUILDKIT=1 docker build --target runtime -t floorline:latest .

image-test:
	DOCKER_BUILDKIT=1 docker build --target test -t floorline:ci .

up:
	docker compose up -d --build

down:
	docker compose down

logs:
	docker compose logs -f --tail=100
