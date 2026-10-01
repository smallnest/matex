package helloworld

import "context"

// GreetArgs / GreetReply are the rpcx method payloads (gob-coded by
// default — no IDL needed, unlike gRPC).
type GreetArgs struct {
	Name string
}

type GreetReply struct {
	Greeting string
	Cached   bool
	Count    int64
}

// RPCServer exposes the domain over rpcx. Registered by the deployment
// unit's RegisterRPCX hook: RegisterName("Greeter", NewRPCServer(...), "").
type RPCServer struct {
	svc *Service
}

// NewRPCServer builds the rpcx service.
func NewRPCServer(s *Service) *RPCServer { return &RPCServer{svc: s} }

// Greet is an rpcx method: func(ctx, *Args, *Reply) error.
func (r *RPCServer) Greet(ctx context.Context, args *GreetArgs, reply *GreetReply) error {
	resp, err := r.svc.Greet(ctx, args.Name)
	if err != nil {
		return err
	}
	reply.Greeting = resp.Greeting
	reply.Cached = resp.Cached
	reply.Count = resp.Count
	return nil
}
