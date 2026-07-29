package cli

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"time"

	"yore/internal/config"
	"yore/internal/daemon"
	"yore/internal/proto"
)

// runSync forces an immediate push/pull cycle and prints the result.
func runSync() int {
	c, err := daemon.EnsureRunning(stateDir())
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore sync:", err)
		return 1
	}
	defer func() { _ = c.Close() }()
	if err := c.Sync(); err != nil {
		fmt.Fprintln(os.Stderr, "yore sync:", err)
		return 1
	}
	st, err := c.Status()
	if err == nil {
		fmt.Printf("sync complete — remote: %s (%d hosts)\n", st.Remote.State, st.Remote.Hosts)
	} else {
		fmt.Println("sync complete")
	}
	return 0
}

// runStatus reports on the daemon and state directory.
func runStatus() int {
	dir := stateDir()
	fmt.Printf("state dir : %s\n", dir)

	conn, err := net.DialTimeout("unix", config.SocketPath(dir), 200*time.Millisecond)
	if err != nil {
		fmt.Println("daemon    : not running (spawns on next recorded command)")
		return 0
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
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
	if st.Remote.State == proto.RemoteRevoked {
		// "revoked" alone reads like a transient. It is not: this device is out
		// of the group, its cached copy of everyone else's history has been
		// deleted, and only re-enrolling changes that.
		fmt.Println("            this device was revoked — its cached remote history has been deleted")
		fmt.Println("            re-enroll it with `yore enroll` to sync again")
	}
	return 0
}
