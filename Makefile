.PHONY: build test check
build:
	go build -o bin/bib ./cmd/bib
test:
	go test -race ./...
check: test
	go vet ./...
