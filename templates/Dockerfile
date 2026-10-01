# syntax=docker/dockerfile:1
# Образ Go-сервиса для homelab. Копируется в корень репозитория сервиса как Dockerfile.
# Пакет main задаётся аргументом CMD; версия из CI попадает в переменную main.version.

FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
ARG CMD=./cmd/bot
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/app "${CMD}"

# distroless/static: корневые сертификаты есть, оболочки и пакетного менеджера нет.
# Часовые пояса сервис вшивает сам импортом time/tzdata.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
USER nonroot:nonroot
ENTRYPOINT ["/app"]
