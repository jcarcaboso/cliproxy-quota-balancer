.PHONY: test build verify image
test:
	go test -race -cover ./...
build:
	mkdir -p dist
	go build -buildmode=c-shared -o dist/quota-balancer.so .
verify: build
	go run ./cmd/verify -plugin-dir "$(CURDIR)/dist"
image:
	docker build -t skorcius/cliproxy-quota-balancer:dev .
