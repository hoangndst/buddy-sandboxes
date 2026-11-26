FROM docker.io/golang:1.26 AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/
COPY pkg/ pkg/
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /buddy-api ./cmd/buddy-api

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /buddy-api /buddy-api
USER 65532:65532
ENTRYPOINT ["/buddy-api"]
