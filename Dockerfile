FROM golang:1.27.2-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /smtp-gateway ./cmd/smtp-gateway \
    && mkdir /data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /smtp-gateway /smtp-gateway
COPY --from=build --chown=nonroot:nonroot /data /data
USER nonroot:nonroot
EXPOSE 8080 2525
ENTRYPOINT ["/smtp-gateway", "serve", "--config", "/etc/smtp-gateway/config.yaml"]
