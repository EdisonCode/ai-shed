PLATFORMS := linux-amd64 linux-arm64 darwin-arm64 darwin-amd64

.PHONY: build test lint dist clean hooks

build:
	go build -o shed ./cmd/shed

test:
	go test ./...

lint:
	test -z "$$(gofmt -l .)"
	go vet ./...

# Run once per clone: the pre-push hook runs the same gates as CI.
hooks:
	git config core.hooksPath .githooks

# One binary per platform, named the way `shed deploy` looks for them.
dist:
	@for p in $(PLATFORMS); do \
		GOOS=$${p%-*} GOARCH=$${p#*-} CGO_ENABLED=0 go build -o dist/shed-$$p ./cmd/shed || exit 1; \
	done

clean:
	rm -rf dist shed
