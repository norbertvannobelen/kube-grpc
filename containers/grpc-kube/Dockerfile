FROM gcr.io/adacado1/go-buildcontainer:1.13 AS build

RUN mkdir -p /go/src/build
ADD src /go/src/build

WORKDIR /go/src/build

# Install Go dependencies then build
RUN go get
RUN GOOS=linux GOARCH=amd64 go build -tags netgo -o app 

FROM gcr.io/adacado1/go-kafkacontainer:1.13

# Add User application
COPY --from=build /go/src/build/app /opt/
CMD /opt/app
