BINARY := floorline
PKG    := ./cmd/floorline

.PHONY: help build test lint run smoke backfill clean

help:
	@echo "make build     compile ./$(BINARY)"
	@echo "make test      go vet + go test ./..."
	@echo "make smoke     probe every Tonnel endpoint (needs a live .env)"
	@echo "make backfill  download trade history"
	@echo "make run       start the pollers and the bot"
	@echo "make clean     remove the binary"

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
