FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/llmgateway ./cmd/gateway

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/llmgateway /app/llmgateway
COPY config/gateway.yaml /app/gateway.yaml
EXPOSE 8080
ENV LLMGATEWAY_CONFIG=/app/gateway.yaml
USER nonroot:nonroot
ENTRYPOINT ["/app/llmgateway"]
