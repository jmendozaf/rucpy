# syntax=docker/dockerfile:1
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/ruc ./cmd/ruc \
    && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ruc /ruc
COPY --from=build --chown=nonroot:nonroot /out/data /data
ENV RUC_DB=/data/ruc.db \
    RUC_ADDR=:8080 \
    RUC_SYNC_EVERY=24h
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/ruc"]
CMD ["serve"]
