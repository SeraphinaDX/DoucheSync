GO ?= go

.PHONY: build test check clean
build:
	CGO_ENABLED=0 $(GO) build -buildvcs=false -trimpath -o DoucheSync .

test:
	$(GO) test -buildvcs=false ./...

check:
	$(GO) test -race -buildvcs=false ./...
	$(GO) vet -buildvcs=false ./...

clean:
	rm -f DoucheSync DoucheSync.exe
