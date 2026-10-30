FROM golang:1.27.3-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /powerchute-exporter ./cmd/powerchute-exporter

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /powerchute-exporter /powerchute-exporter
USER 65532:65532
EXPOSE 9787
ENTRYPOINT ["/powerchute-exporter"]
CMD ["-config", "/etc/powerchute/config.yaml"]
