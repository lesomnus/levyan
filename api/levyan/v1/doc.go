// Package levyanv1 is what an app calls levyan with: the messages and the
// gRPC clients of `levyan.v1`, generated from the .proto beside them.
//
// It is of a module of its own (`github.com/lesomnus/levyan/api`) that
// requires gRPC and protobuf and nothing of levyan's, so that an app calling
// levyan is held to neither levyan's schema nor the payday levyan is built
// with. An app in another language generates its own from the same .proto.
//
//	conn, err := grpc.NewClient(addr, creds)
//	books := levyanv1.NewPriceBookServiceClient(conn)
//	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+key)
//	ms, err := books.ListMeters(ctx, &levyanv1.ListMetersRequest{})
package levyanv1
