#build stage
FROM golang:alpine AS builder
RUN apk add --no-cache git
WORKDIR /go/src/app
COPY . .
RUN go get -d -v ./...
RUN go build -o /go/bin/app -v ./...

#final stage
FROM alpine:latest
RUN apk --no-cache add ca-certificates
COPY --from=builder /go/bin/app /app
COPY templates ./templates
ENTRYPOINT /app
LABEL Name=pongstrong Version=1.0.0
EXPOSE 8080

ENV SESSION_KEY=roesitown
ENV APP_PWD=bierhefe
ENV CTRLPANEL_PWD=pusteblume
ENV BACKUP_TIME=2
ENV RESETABLE=true