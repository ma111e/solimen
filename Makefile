.PHONY: build snapshot check help

default: help

## build    : Build the solimen binary
build:
	go build ./cmd/solimen

## snapshot : Build binaries and the Docker image locally via GoReleaser (no publish)
snapshot:
	goreleaser release --snapshot --clean

## check    : Validate the GoReleaser configuration
check:
	goreleaser check

## help     : Shows this help
help: Makefile
	@printf ">] SOLIMEN\n\n"
	@sed -n 's/^##//p' $< | column -t -s ':' | sed -e 's/^/ /'
