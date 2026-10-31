// What an app calls levyan with (docs/design.md, "앱이 levyan을 부르는 길"):
// the API's .proto and the Go generated from it, and nothing of levyan's own.
//
// A module of its own so that an app that requires it requires nothing else of
// levyan's -- above all not the payday levyan is built with, which would be
// the version of payday the app had to be on. So it requires gRPC and
// protobuf and no more, and `scripts/api.sh --check` refuses it anything else.
module github.com/lesomnus/levyan/api

go 1.25.0

require (
	google.golang.org/grpc v1.83.2
	google.golang.org/protobuf v1.36.12
)

require (
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
)
