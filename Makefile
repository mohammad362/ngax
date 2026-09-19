IMAGE := ngax-dev
RUN   := docker run --rm -v "$(CURDIR)":/app -v ngax-gocache:/root/.cache -v ngax-gomod:/go/pkg/mod -w /app $(IMAGE)

.PHONY: image test vet fmt build sh bench tidy

image:
	docker build -q -f Dockerfile.dev -t $(IMAGE) . >/dev/null

test: image
	$(RUN) go test -count=1 ./...

vet: image
	$(RUN) sh -c 'gofmt -l . && go vet ./...'

fmt: image
	$(RUN) gofmt -w .

build: image
	$(RUN) go build -buildvcs=false -ldflags='-s -w' -o ngax .

bench: image
	$(RUN) go test -run '^$$' -bench . -benchmem ./...

tidy: image
	$(RUN) go mod tidy

sh: image
	docker run --rm -it -v "$(CURDIR)":/app -v ngax-gocache:/root/.cache -v ngax-gomod:/go/pkg/mod -w /app $(IMAGE) sh
