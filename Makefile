.PHONY: build image push build-push help

default: help

## build : Build the solimen binary
build:
	go build ./cmd/solimen

## image : Build and increment solimen Docker image version
image:
	@VERSION=$$(cat docker/.version); \
	MAJOR=$$(echo $$VERSION | cut -d. -f1); \
	MINOR=$$(echo $$VERSION | cut -d. -f2); \
	PATCH=$$(echo $$VERSION | cut -d. -f3); \
	NEW_VERSION=$$MAJOR.$$MINOR.$$((PATCH + 1)); \
	echo "Building solimen:$$NEW_VERSION..."; \
	docker build -t ghcr.io/ma111e/solimen:latest -t ghcr.io/ma111e/solimen:$$NEW_VERSION --build-arg VERSION=$$NEW_VERSION -f ./docker/Dockerfile . && \
	echo $$NEW_VERSION > docker/.version && \
	echo "✓ Built and tagged as $$NEW_VERSION"

## push : Push solimen Docker image to registry
push:
	@VERSION=$$(cat docker/.version); \
	echo "Pushing ghcr.io/ma111e/solimen:latest and :$$VERSION..."; \
	docker push ghcr.io/ma111e/solimen:latest && \
	docker push ghcr.io/ma111e/solimen:$$VERSION && \
	echo "✓ Pushed solimen:$$VERSION"

## build-push : Build, increment version, and push solimen Docker image
build-push: image push

## help : Shows this help
help: Makefile
	@printf ">] SOLIMEN\n\n"
	@sed -n 's/^##//p' $< | column -t -s ':' | sed -e 's/^/ /'
