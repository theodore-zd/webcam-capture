FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/webcam .

FROM alpine:3.20
RUN apk add --no-cache ffmpeg tzdata ca-certificates
COPY --from=build /out/webcam /usr/local/bin/webcam
ENTRYPOINT ["webcam"]
