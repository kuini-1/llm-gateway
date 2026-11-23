FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mockllm ./cmd/mockllm \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gwbench ./cmd/gwbench

FROM alpine:3.22
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 app
COPY --from=build /out/ /usr/local/bin/
USER app
EXPOSE 8080
HEALTHCHECK --interval=15s --timeout=3s CMD wget -qO- http://127.0.0.1:8080/readyz || exit 1
ENTRYPOINT ["gateway"]
CMD ["serve", "-config", "/etc/gateway/gateway.yaml"]
