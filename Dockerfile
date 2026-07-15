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
# Empty dir used to seed the /data mountpoint below with nonroot ownership.
RUN mkdir -p /seed

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /connector /connector
# Durable state (delta cursor + pending-retry sets) lives at /data, backed by a
# mounted Fly volume so it survives restarts. Seed the mountpoint owned by the
# distroless nonroot user (uid 65532) so the non-root process can write it when
# Fly first mounts an (empty) volume there.
COPY --from=build --chown=65532:65532 /seed /data
WORKDIR /data
ENV PORT=8080 \
    GRAPH_DELTA_TOKEN_FILE=/data/graph_delta_link
EXPOSE 8080
ENTRYPOINT ["/connector"]
CMD ["serve"]
