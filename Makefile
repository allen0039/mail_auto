.PHONY: build test check run image
build:
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -o bin/mail-auto ./cmd/mail-auto
run:
	go run ./cmd/mail-auto
test:
	go test ./...
check:
	go test -race ./...
	go vet ./...
	@test -z "$$(gofmt -l .)" || (gofmt -l . && false)
	go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...
image:
	docker compose build
