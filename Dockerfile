FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/proxy ./cmd/proxy

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/proxy /usr/local/bin/proxy
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/proxy"]
