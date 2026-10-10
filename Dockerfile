FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod main.go ./
COPY web ./web
COPY data ./data
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /calendar .
FROM scratch
COPY --from=build /calendar /calendar
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
USER 10001:10001
ENV DATA_DIR=/data LISTEN_ADDR=:8080
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s CMD ["/calendar", "healthcheck"]
ENTRYPOINT ["/calendar"]
