FROM quay.io/cdis/golang:1.12-alpine as build

# Install SSL certificates
RUN apk update && apk add --no-cache git ca-certificates gcc musl-dev

# Build static sidecar binary
RUN mkdir -p /go/src/github.com/uc-cdis/mariner
WORKDIR /go/src/github.com/uc-cdis/mariner
ADD . .
RUN go build -ldflags "-linkmode external -extldflags -static" -o /sidecar ./sidecar/

# we love small images
FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /sidecar /

ENTRYPOINT ["/sidecar"]
