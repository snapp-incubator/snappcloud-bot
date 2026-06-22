.PHONY: build test lint run docker
build:
	go build -o bin/snappcloud-bot ./cmd/snappcloud-bot
test:
	go test -race ./...
lint:
	golangci-lint run ./...
run:
	go run ./cmd/snappcloud-bot -config config.example.yaml
docker:
	docker buildx bake -f build/package/docker-bake.json
