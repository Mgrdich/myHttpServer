vet:
	go vet ./...

lint: vet
	golangci-lint run

test:
	go test -v ./...

build_ci:
	CGO_ENABLED=0 GOOS=linux go build -o server cmd/main.go

install:
	go mod download

tidy:
	go mod tidy
