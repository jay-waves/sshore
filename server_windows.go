package main

func serveRemoteRequest(req remoteServiceRequest) error {
	switch req.Operation {
	case "shell":
		return serveRemote(req)
	case "cp-agent", "mount-agent":
		return serveRemoteFileService(req, beginCommandActivity)
	case "forward-agent":
		return serveForwardAgent(req)
	case "task-control":
		if err := checkPeerVersion(req.Release, "client"); err != nil {
			return err
		}
		return serveTaskControl(req)
	default:
		return usage("unknown remote service operation")
	}
}
