# Development bootstrap image. Release hardening belongs to bablo-deploy.
FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/bablo ./cmd/bablo

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/bablo /bablo
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/bablo"]
