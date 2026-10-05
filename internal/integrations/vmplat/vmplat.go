package vmplat

import (
	"context"
	"strconv"
	"strings"

	"github.com/hackclub/ari-webhooks/internal/httpx"
)

type Client struct {
	BaseUrl string
	Token   string
}

func (c *Client) Configured() bool {
	return strings.TrimSpace(c.Token) != "" && c.base() != ""
}

func (c *Client) base() string {
	return strings.TrimSuffix(strings.TrimSpace(c.BaseUrl), "/")
}

// DeleteVm mirrors ari's vm.ts deleteVm: 404 counts as success so a stale row still clears.
func (c *Client) DeleteVm(ctx context.Context, vmid int) bool {
	if !c.Configured() {
		return false
	}
	res := httpx.Json(ctx, c.base()+"/vms/"+strconv.Itoa(vmid), httpx.Opts{
		Method:    "DELETE",
		Headers:   map[string]string{"Authorization": "Bearer " + c.Token},
		TimeoutMs: 90000, // the platform can be slow while provisioning elsewhere
	})
	return res.OK || res.Status == 404
}
