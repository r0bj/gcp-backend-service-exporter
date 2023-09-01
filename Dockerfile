FROM golang:1.21.0 AS builder

WORKDIR /workspace

COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o gcp-backend-service-exporter .


FROM scratch

COPY --from=builder /workspace/gcp-backend-service-exporter /

USER 65534:65534

ENTRYPOINT ["/gcp-backend-service-exporter"]
