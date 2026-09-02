package cache

import (
	"bufio"
	"io"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

type Client struct {
	addr string
	mu   sync.RWMutex
	mem  map[string]map[float64]string
	memKV map[string]string
	available bool
}

func New(redisURL string) *Client {
	addr := "localhost:6379"
	if strings.HasPrefix(redisURL, "redis://") {
		h := strings.TrimPrefix(redisURL, "redis://")
		if idx := strings.Index(h, "/"); idx != -1 {
			h = h[:idx]
		}
		if h != "" {
			addr = h
		}
		if !strings.Contains(addr, ":") {
			addr += ":6379"
		}
	} else if redisURL != "" && strings.Contains(redisURL, ":") {
		addr = redisURL
	}
	c := &Client{addr: addr, mem: map[string]map[float64]string{}, memKV: map[string]string{}}
	// probe
	conn, err := net.DialTimeout("tcp", addr, 800*time.Millisecond)
	if err == nil {
		c.available = true
		conn.Close()
	}
	return c
}

func (c *Client) Available() bool { return c.available }

func (c *Client) ZAdd(key string, score float64, member string) error {
	if c.available {
		if err := c.redisCmd("ZADD", key, fmt.Sprintf("%f", score), member); err == nil {
			return nil
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mem[key] == nil {
		c.mem[key] = map[float64]string{}
	}
	c.mem[key][score] = member
	return nil
}

func (c *Client) ZRange(key string) ([]string, error) {
	if c.available {
		if vals, err := c.redisZRange(key); err == nil {
			return vals, nil
		}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []string
	for _, v := range c.mem[key] {
		out = append(out, v)
	}
	return out, nil
}

func (c *Client) Ping() error {
	if !c.available {
		return fmt.Errorf("redis not available (fallback memory active)")
	}
	return c.redisCmd("PING")
}

func (c *Client) Get(key string) (string, error) {
	if c.available {
		if v, err := c.redisGet(key); err == nil {
			return v, nil
		}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if v, ok := c.memKV[key]; ok {
		return v, nil
	}
	return "", fmt.Errorf("key not found: %s", key)
}

func (c *Client) Set(key, value string) error {
	return c.SetEx(key, value, 0)
}

func (c *Client) SetEx(key, value string, ttl time.Duration) error {
	if c.available {
		args := []string{"SET", key, value}
		if ttl > 0 {
			args = []string{"SET", key, value, "EX", fmt.Sprintf("%d", int(ttl.Seconds()))}
		}
		if err := c.redisCmd(args...); err == nil {
			return nil
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.memKV[key] = value
	return nil
}

func (c *Client) Del(key string) error {
	if c.available {
		_ = c.redisCmd("DEL", key)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.memKV, key)
	return nil
}

func (c *Client) redisGet(key string) (string, error) {
	conn, err := net.DialTimeout("tcp", c.addr, 800*time.Millisecond)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	cmd := fmt.Sprintf("*2\r\n$3\r\nGET\r\n$%d\r\n%s\r\n", len(key), key)
	if _, err := conn.Write([]byte(cmd)); err != nil {
		return "", err
	}
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "$-1" {
		return "", fmt.Errorf("nil")
	}
	if strings.HasPrefix(line, "-") {
		return "", fmt.Errorf("redis error: %s", line)
	}
	if !strings.HasPrefix(line, "$") {
		return "", fmt.Errorf("unexpected reply: %s", line)
	}
	n := 0
	fmt.Sscan(line[1:], &n)
	buf := make([]byte, n+2)
	if _, err := io.ReadFull(br, buf); err != nil {
		return "", err
	}
	return string(buf[:n]), nil
}


func (c *Client) redisCmd(args ...string) error {
	conn, err := net.DialTimeout("tcp", c.addr, 800*time.Millisecond)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	var b strings.Builder
	b.WriteString(fmt.Sprintf("*%d\r\n", len(args)))
	for _, a := range args {
		b.WriteString(fmt.Sprintf("$%d\r\n%s\r\n", len(a), a))
	}
	if _, err := conn.Write([]byte(b.String())); err != nil {
		return err
	}
	// read reply (ignore)
	br := bufio.NewReader(conn)
	line, _ := br.ReadString('\n')
	if strings.HasPrefix(line, "-") {
		return fmt.Errorf("redis error: %s", line)
	}
	return nil
}

func (c *Client) redisZRange(key string) ([]string, error) {
	conn, err := net.DialTimeout("tcp", c.addr, 800*time.Millisecond)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	cmd := fmt.Sprintf("*2\r\n$6\r\nZRANGE\r\n$%d\r\n%s\r\n$5\r\n0 -1\r\n", len(key), key)
	if _, err := conn.Write([]byte(cmd)); err != nil {
		return nil, err
	}
	br := bufio.NewReader(conn)
	line, _ := br.ReadString('\n')
	if strings.HasPrefix(line, "-") {
		return nil, fmt.Errorf("redis error: %s", line)
	}
	return nil, fmt.Errorf("not implemented full parse, fallback")
}
