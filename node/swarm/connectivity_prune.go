package swarm

import "time"

// Bound unverified relay-asserted identities as well as setup workers. Expire
// unused slots, including a negotiated leg that never authenticated inner TLS.
func (c *connectivity) prune() {
	timer := time.NewTicker(15 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case now := <-timer.C:
			c.mu.Lock()
			for id, p := range c.peers {
				if now.Sub(p.touched) < time.Minute {
					continue
				}
				if _, live := c.s.host.Connection(id); live {
					continue
				}
				select {
				case p.gate <- struct{}{}:
					if p.leg != nil {
						p.leg.Close()
						p.leg = nil
					}
					c.socket.RemovePeer(id)
					delete(c.peers, id)
					p.unlock()
				default:
				}
			}
			c.mu.Unlock()
			if c.network != nil {
				c.network.ensureControl()
			}
		}
	}
}
