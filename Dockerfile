# Build the Go connector and ship it as a minimal static binary (no interpreter,
# no source) — keeps the distributed listener closed and the image tiny.

FROM golang:1.23-alpine AS build
WORKDIR /src
# The Goodmem SDK (fury.io/pairsys/goodmem) is served from Gemfury's public
# tokenless proxy; sum.golang.org doesn't index it, so disable the sum DB.
ENV GOPROXY=https://go-proxy.fury.io/pairsys/,https://proxy.golang.org,direct \
    GOSUMDB=off \
    CGO_ENABLED=0 \
    GOOS=linux
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /connector ./cmd/connector

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /connector /connector
# /tmp is writable for the nonroot user; the delta cursor lives there.
WORKDIR /tmp
ENV PORT=8080 \
    GRAPH_DELTA_TOKEN_FILE=/tmp/graph_delta_link
EXPOSE 8080
ENTRYPOINT ["/connector"]
CMD ["serve"]
