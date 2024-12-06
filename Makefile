vet:
	go vet ./...

lint: vet
	golangci-lint run

test:
	go test -v ./...

build:
	CGO_ENABLED=0 go build -o main-exec cmd/main.go

build_ci:
	CGO_ENABLED=0 GOOS=linux go build -o main-exec cmd/main.go

install:
	go mod download

tidy:
	go mod tidy

run:
	go run cmd/main.go