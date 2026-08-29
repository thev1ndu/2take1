FROM golang:1.25-alpine AS build
WORKDIR /src

COPY go.mod ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /out/mcp-server .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/mcp-server /usr/local/bin/mcp-server

EXPOSE 8081
USER nobody
ENTRYPOINT ["/usr/local/bin/mcp-server"]
