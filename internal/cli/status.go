package cli

import (
	"bufio"
	"fmt"
	"net"
	"time"

	"yore/internal/config"
	"yore/internal/proto"
)

// cmdStatus reports on the daemon and state directory.
func cmdStatus([]string) int {
	dir := stateDir()
	fmt.Printf("state dir : %s\n", dir)

	conn, err := net.DialTimeout("unix", config.SocketPath(dir), 200*time.Millisecond)
	if err != nil {
		fmt.Println("daemon    : not running (spawns on next recorded command)")
		return 0
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	if err := proto.WriteMsg(conn, proto.Request{Op: proto.OpStatus}); err != nil {
		fmt.Println("daemon    : unreachable:", err)
		return 1
	}
	var resp proto.Response
	if err := proto.ReadMsg(bufio.NewReader(conn), &resp); err != nil || !resp.OK || resp.Status == nil {
		fmt.Println("daemon    : bad response")
		return 1
	}
	st := resp.Status
	fmt.Printf("daemon    : running (pid %d, up %s, %s)\n",
		st.PID, (time.Duration(st.UptimeSec) * time.Second).String(), st.Version)
	fmt.Printf("history   : %d local entries\n", st.LocalRows)
	fmt.Printf("remote    : %s\n", st.Remote.State)
	return 0
}
