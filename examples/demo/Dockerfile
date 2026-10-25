# Build context is the repository root (see examples/docker-compose.yml).
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY . .
WORKDIR /src/examples
RUN CGO_ENABLED=0 go build -o /out/demo ./demo

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/demo /demo
ENTRYPOINT ["/demo"]
