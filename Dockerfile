# Development bootstrap image. Release hardening belongs to bablo-deploy.
FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd ./cmd
COPY internal ./internal
COPY migrations ./migrations
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/bablo ./cmd/bablo \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/bablo-migrate ./cmd/bablo-migrate

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/bablo /bablo
COPY --from=build /out/bablo-migrate /bablo-migrate
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/bablo"]
