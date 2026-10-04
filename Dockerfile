FROM golang:1.26.5-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go test ./... \
 && go build -buildmode=c-shared -o /out/quota-balancer.so . \
 && go build -o /out/configure-quota-balancer ./cmd/configure \
 && go build -o /out/verify-quota-balancer ./cmd/verify \
 && host="$(go list -m -f '{{.Dir}}' github.com/router-for-me/CLIProxyAPI/v8)" \
 && cd "$host" \
 && go build -buildvcs=false -ldflags="-X main.Version=quota-balancer-host -X main.Commit=43b0f3d936401f0c449313886108e6a554dbd5be" -o /out/CLIProxyAPI ./cmd/server

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates tzdata \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /CLIProxyAPI
COPY --from=build /out/CLIProxyAPI ./
COPY --from=build /out/quota-balancer.so ./plugins/quota-balancer.so
COPY --from=build /out/configure-quota-balancer /usr/local/bin/configure-quota-balancer
COPY --from=build /out/verify-quota-balancer /usr/local/bin/verify-quota-balancer
COPY policy.yaml /etc/quota-balancer.yaml
COPY --chmod=755 docker-entrypoint.sh /usr/local/bin/quota-balancer-entrypoint
RUN /usr/local/bin/verify-quota-balancer -plugin-dir /CLIProxyAPI/plugins
EXPOSE 8317
ENTRYPOINT ["/usr/local/bin/quota-balancer-entrypoint"]
