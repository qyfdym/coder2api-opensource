package qoder

import "crypto/tls"

// tlsConn 仅用于给 Transport.TLSNextProto 写出非 nil 的空 map：
// 空 map 会让 net/http 关闭 HTTP/2 自动协商，固定走 HTTP/1.1。
type tlsConn = tls.Conn
