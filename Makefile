BINARY := floorline
PKG    := ./cmd/floorline

.PHONY: help build test lint run smoke backfill clean image image-test up down logs deploy

help:
	@echo "make build     compile ./$(BINARY)"
	@echo "make test      go vet + go test ./..."
	@echo "make smoke     probe every Tonnel endpoint (needs a live .env)"
	@echo "make backfill  download trade history"
	@echo "make run       start the pollers and the bot"
	@echo "make clean     remove the binary"
	@echo "make image     build the production container (tests run inside)"
	@echo "make deploy    build here, ship the image, switch with rollback"
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

# The runtime stage takes its binary from the test stage, so the suite runs on
# every path that produces a deployable image — including `docker compose
# build`, which targets runtime directly and used to skip it entirely.
image:
	DOCKER_BUILDKIT=1 docker build --target runtime -t floorline:latest .

image-test:
	DOCKER_BUILDKIT=1 docker build --target test -t floorline:ci .

# Compiling on the 2 GB production host pushes it into swap and takes the
# neighbouring services with it. Build here, ship the finished image.
deploy:
	./deploy/ship.sh $(HOST) $(DIR)

up:
	docker compose up -d --build

down:
	docker compose down

logs:
	docker compose logs -f --tail=100
