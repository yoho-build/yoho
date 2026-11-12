FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 go build -o /allow .
FROM alpine:3.23
COPY --from=build /allow /usr/local/bin/allow
USER 65534:65534
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/allow"]
