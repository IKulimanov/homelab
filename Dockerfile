# syntax=docker/dockerfile:1
# Образ платформенных программ homelab: ops-bot и llm-gateway. Какую собрать — задаёт аргумент CMD
# (.github/workflows/build.yml). Копия templates/Dockerfile; меняя одно, проверь другое.

FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
ARG CMD=./cmd/ops-bot
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/app "${CMD}"

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
USER nonroot:nonroot
ENTRYPOINT ["/app"]
