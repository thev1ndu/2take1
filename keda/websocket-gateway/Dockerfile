FROM golang:1.25-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /out/websocket-gateway .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/websocket-gateway /usr/local/bin/websocket-gateway

EXPOSE 8082
USER nobody
ENTRYPOINT ["/usr/local/bin/websocket-gateway"]
