FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 go build -trimpath -o /out/app .

FROM alpine:3.22
RUN apk add --no-cache wget
COPY --from=build /out/app /app
EXPOSE 8080
STOPSIGNAL SIGTERM
CMD ["/app"]
