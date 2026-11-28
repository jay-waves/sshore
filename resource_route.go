package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"slices"

	"github.com/jay-waves/sshore/internal/taskid"
)

func approvedResourceRequest(session string, pid int, id string, op operation) message {
	return message{Type: "route_resource", Name: session, PID: pid, ApprovalID: id, Operations: []operation{op}}
}

// Short file transfers can reuse the authenticated attachment path.
func approvedFileOptions(connection recentConnection, attachment *options) (options, error) {
	if err := validateRecentConnection(connection); err != nil {
		return options{}, err
	}
	if !taskid.Valid(connection.Instance) {
		return options{}, fmt.Errorf("routed resource requires a live shell route")
	}
	o := options{command: "attach", host: connection.Host, session: connection.Session}
	applyConnectionOptions(&o, connection, attachment)
	return o, nil
}

func openIndependentResource(ctx context.Context, connection recentConnection, request message, service *resourceService) (net.Conn, error) {
	o, err := independentResourceRoute(connection, &request)
	if err != nil {
		return nil, err
	}
	var c net.Conn
	if o.hosted {
		c, err = openClientTransportContext(ctx, &o, nil)
	} else {
		o.environment = service.baseEnvironment()
		c, err = openRemoteShellTransport(ctx, &o, nil, service.auth)
	}
	if err != nil {
		return nil, err
	}
	return negotiateResourceEndpoint(ctx, c, request)
}

func independentResourceRoute(connection recentConnection, request *message) (options, error) {
	if err := validateRecentConnection(connection); err != nil {
		return options{}, err
	}
	request.Instance = connection.Instance
	root := &connection
	for ; root.Parent != nil; root = root.Parent {
		request.ResourceRoute = append(request.ResourceRoute, resourceHop{Parent: root.Parent.Instance, Token: root.RouteToken})
	}
	slices.Reverse(request.ResourceRoute)
	o := routeParentOptions(options{command: "attach", host: root.Host, session: root.Session, instance: root.Instance})
	applyConnectionOptions(&o, *root, nil)
	return o, nil
}

func openResourceEndpoint(ctx context.Context, o *options, request message, transports clientTransports) (_ net.Conn, err error) {
	request.Instance = o.instance
	c, err := openClientTransportContext(ctx, o, transports)
	if err != nil {
		return nil, err
	}
	return negotiateResourceEndpoint(ctx, c, request)
}

func negotiateResourceEndpoint(ctx context.Context, c net.Conn, request message) (_ net.Conn, err error) {
	defer func() {
		if err != nil {
			c.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	reply, err := exchangeRouteRequest(c, request)
	if !stop() {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	if err := checkPeerVersion(reply.SoftwareVersion, "resource daemon"); err != nil {
		return nil, err
	}
	if reply.Type != "resource_ready" {
		return nil, fmt.Errorf("routed resource: %s", reply.Error)
	}
	return c, nil
}

func openRemoteTaskEndpoint(ctx context.Context, record localShellRecord, dial func(context.Context, string) (net.Conn, error)) (net.Conn, error) {
	channel, err := dial(ctx, record.Address)
	if err != nil {
		return nil, err
	}
	return upgradeDaemonConn(ctx, channel, record, "/tasks/"+record.UID+"/open", nil)
}

// Either direction ending closes the entire task or resource route.
func relayRoute(a, b io.ReadWriteCloser) {
	copied := make(chan struct{}, 2)
	copyStream := func(dst, src io.ReadWriteCloser) {
		_, _ = io.Copy(dst, src)
		copied <- struct{}{}
	}
	go copyStream(a, b)
	go copyStream(b, a)
	<-copied

	// Start both closes independently. A protocol close can block on I/O.
	closed := make(chan struct{})
	go func() {
		_ = b.Close()
		close(closed)
	}()
	_ = a.Close()
	<-closed
	<-copied
}
