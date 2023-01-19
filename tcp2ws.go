// Tcp over WebSocket (tcp2ws)
// 基于ws的内网穿透工具
// Sparkle 20210430
// v8.3

package main

import (
	"github.com/gorilla/websocket"
	"github.com/google/uuid"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"fmt"
	"regexp"
	"time"
	"os/signal"
	"sync"
	"crypto/tls"
)

type tcp2wsSparkle struct {
	tcpConn net.Conn
	wsConn *websocket.Conn
	uuid string
	del bool
	buf [][]byte
	t int64
 }

var (
	tcpAddr string
	wsAddr string
	wsAddrIp string
	wsAddrPort = ""
	msgType int = websocket.BinaryMessage
	isServer bool
	connMap map[string]*tcp2wsSparkle = make(map[string]*tcp2wsSparkle)
	// go的map不是线程安全的 读写冲突就会直接exit
	connMapLock *sync.RWMutex = new(sync.RWMutex)
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool{ return true },
}

func getConn(uuid string) (*tcp2wsSparkle, bool) {
	connMapLock.RLock()
	defer connMapLock.RUnlock()
	conn, haskey := connMap[uuid];
	return conn, haskey
}

func setConn(uuid string, conn *tcp2wsSparkle) {
	connMapLock.Lock()
	defer connMapLock.Unlock()
	connMap[uuid] = conn
}

func deleteConn(uuid string) {
	if conn, haskey := getConn(uuid); haskey && conn != nil && !conn.del{
		connMapLock.Lock()
		defer connMapLock.Unlock()
		conn.del = true
		if conn.tcpConn != nil {
			conn.tcpConn.Close()
		}
		if conn.wsConn != nil {
			log.Print(uuid, " bye")
			conn.wsConn.WriteMessage(websocket.TextMessage, []byte("tcp2wsSparkleClose"))
			conn.wsConn.Close()
		}
		delete(connMap, uuid)
	}
	// panic("炸一下试试")
}

func ReadTcp2Ws(uuid string) (bool) {
	defer func() {
		err := recover()
		if err != nil {
			log.Print(uuid, " tcp -> ws Boom!\n", err)
			ReadTcp2Ws(uuid)
		}
	}()

	conn, haskey := getConn(uuid);
	if !haskey {
		return false
	}
	buf := make([]byte, 500000)
	tcpConn := conn.tcpConn
	for {
		if conn.del || tcpConn == nil {
			return false
		}
		length,err := tcpConn.Read(buf)
		if err != nil {
			if conn, haskey := getConn(uuid); haskey && !conn.del {
				// tcp中断 关闭所有连接 关过的就不用关了
				if err.Error() != "EOF" {
					log.Print(uuid, " tcp read err: ", err)
				}
				deleteConn(uuid)
				return false
			}
			return false
		}
		if length > 0 {
			// 因为tcpConn.Read会阻塞 所以要从connMap中获取最新的wsConn
			conn, haskey := getConn(uuid);
			if !haskey || conn.del {
				return false
			}
			wsConn := conn.wsConn
			if wsConn == nil {
				return false
			}
			conn.t = time.Now().Unix()
			if err = wsConn.WriteMessage(msgType, buf[:length]);err != nil{
				log.Print(uuid, " ws write err: ", err)
				// tcpConn.Close()
				wsConn.Close()
				// save send error buf
				if conn.buf == nil{
					conn.buf = [][]byte{buf[:length]}
				} else {
					conn.buf = append(conn.buf, buf[:length])
				}
				// 此处无需中断 等着新的wsConn 或是被 断开连接 / 回收 即可
			}
			// if !isServer {
			// 	log.Print(uuid, " send: ", length)	
			// }
		}
	}
}

func ReadWs2Tcp(uuid string) (bool) {
	defer func() {
		err := recover()
		if err != nil {
			log.Print(uuid, " ws -> tcp Boom!\n", err)
			ReadWs2Tcp(uuid)
		}
	}()

	conn, haskey := getConn(uuid);
	if !haskey {
		return false
	}
	wsConn := conn.wsConn
	tcpConn := conn.tcpConn
	for {
		if conn.del || tcpConn == nil || wsConn == nil {
			return false
		}
		t, buf, err := wsConn.ReadMessage()
		if err != nil || t == -1 {
			wsConn.Close()
			if conn, haskey := getConn(uuid); haskey && !conn.del {
				// 外部干涉导致中断 重连ws
				log.Print(uuid, " ws read err: ", err)
				return true
			}
			return false
		}
		if len(buf) > 0 {
			conn.t = time.Now().Unix()
			if t == websocket.TextMessage {
				msg := string(buf)
				if msg == "tcp2wsSparkle" {
					log.Print(uuid, " 咩")
					continue
				} else if msg == "tcp2wsSparkleClose" {
					log.Print(uuid, " say bye")
					connMapLock.Lock()
					defer connMapLock.Unlock()
					wsConn.Close()
					tcpConn.Close()
					delete(connMap, uuid)
					return false
				}
			}
			msgType = t
			if _, err = tcpConn.Write(buf);err != nil{
				log.Print(uuid, " tcp write err: ", err)
				deleteConn(uuid)
				return false
			}
			// if !isServer {
			// 	log.Print(uuid, " recv: ", len(buf))	
			// }
		}
	}
}

func ReadWs2TcpClient(uuid string) {
	if ReadWs2Tcp(uuid) {
		// error return  re call ws
		RunClient(nil, uuid)
	}
}

func writeErrorBuf2Ws(uuid string)  {
	if conn, haskey := getConn(uuid); haskey && conn.buf != nil {
		for i := 0; i < len(conn.buf); i++ {
			conn.wsConn.WriteMessage(websocket.BinaryMessage, conn.buf[i])
		}
		conn.buf = nil
	}
}

// 自定义的Dial连接器，自定义域名解析
func MeDial(network, address string) (net.Conn, error) {
	// return net.DialTimeout(network, address, 5 * time.Second)
	return net.DialTimeout(network, wsAddrIp + wsAddrPort, 5 * time.Second)
}

func RunServer(wsConn *websocket.Conn) {
	defer func() {
		err := recover()
		if err != nil {
			log.Print("server Boom!\n", err)
		}
	}()

	var tcpConn net.Conn
	var uuid string
	// read uuid to get from connMap
	t, buf, err := wsConn.ReadMessage()
	if err != nil || t == -1 {
		log.Print(" ws uuid read err: ", err)
		wsConn.Close()
		return
	}
	if len(buf) > 0 {
		if t == websocket.TextMessage {
			uuid = string(buf)
			// get
			if conn, haskey := getConn(uuid); haskey {
				tcpConn = conn.tcpConn
				conn.wsConn.Close()
				conn.wsConn = wsConn
				writeErrorBuf2Ws(uuid)
			}
		}
	}

	if tcpConn == nil {
		log.Print("new tcp for ", uuid)
		// call tcp
		tcpConn, err = net.Dial("tcp", tcpAddr)
		if(err != nil) {
			log.Print("connect to tcp err: ", err)
			return
		}
		if uuid != "" {
			// save
			setConn(uuid, &tcp2wsSparkle {tcpConn, wsConn, uuid, false, nil, time.Now().Unix()})
		}

		go ReadTcp2Ws(uuid)
	} else {
		log.Print("uuid finded ", uuid)
	}
	
	go ReadWs2Tcp(uuid)
}

func RunClient(tcpConn net.Conn, uuid string) {
	defer func() {
		err := recover()
		if err != nil {
			log.Print("client Boom!\n", err)
		}
	}()
	// conn is close?
	if tcpConn == nil {
		if conn, haskey := getConn(uuid); haskey {
			if conn.del {
				return
			}
		} else {
			return
		}
	}
	log.Print(uuid, " dial")
	// call ws
	dialer := websocket.Dialer{TLSClientConfig: &tls.Config{RootCAs: nil, InsecureSkipVerify: true}, Proxy: http.ProxyFromEnvironment, NetDial: MeDial}
	wsConn, _, err := dialer.Dial(wsAddr, nil)
	if err != nil {
		log.Print("connect to ws err: ", err)
		if tcpConn != nil {
			tcpConn.Close()
		}
		return
	}
	// send uuid
	if err := wsConn.WriteMessage(websocket.TextMessage, []byte(uuid));err != nil{
		log.Print("send ws uuid err: ", err)
		if tcpConn != nil {
			tcpConn.Close()
		}
		wsConn.Close()
		return
	}
	
	// save conn
	if tcpConn != nil {
		// save
		setConn(uuid, &tcp2wsSparkle {tcpConn, wsConn, uuid, false, nil, time.Now().Unix()})
	} else {
		// update
		if conn, haskey := getConn(uuid); haskey {
			conn.wsConn.Close()
			conn.wsConn = wsConn
			conn.t = time.Now().Unix()
			writeErrorBuf2Ws(uuid)
		}
	}

	go ReadWs2TcpClient(uuid)
	if tcpConn != nil {
		go ReadTcp2Ws(uuid)
	}
}



// 响应ws请求
func wsHandler(w http.ResponseWriter , r *http.Request){
	forwarded := r.Header.Get("X-Forwarded-For")
	// 不是ws的请求返回index.html 假装是一个静态服务器
	if r.Header.Get("Upgrade") != "websocket" {
		if forwarded == "" {
			log.Print("not ws: ", r.RemoteAddr)
		} else {
			log.Print("not ws: ", forwarded)
		}
		_, err := os.Stat("index.html")
		if err == nil {
			http.ServeFile(w, r, "index.html")
		}
		return
	} else {
		if forwarded == "" {
			log.Print("new ws conn: ", r.RemoteAddr)
		} else {
			log.Print("new ws conn: ", forwarded)
		}
	}

	// ws协议握手
	conn, err := upgrader.Upgrade(w,r,nil)
	if err != nil{
		log.Print("ws upgrade err: ", err)
		return 
	}

	// 新线程hold住这条连接
	go RunServer(conn) 
}

// 响应tcp
func tcpHandler(listener net.Listener){
	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Print("tcp accept err: ", err)
			return
		}

		log.Print("new tcp conn: ")

		// 新线程hold住这条连接
		go RunClient(conn, uuid.New().String()[32:])
	}
}

// 启动ws服务
func startWsServer(listenPort string, isSsl bool, sslCrt string, sslKey string){
	var err error = nil
	if isSsl {
		fmt.Println("use ssl cert: " + sslCrt + " " + sslKey)
		err = http.ListenAndServeTLS(listenPort, sslCrt, sslKey, nil)
	} else {
		err = http.ListenAndServe(listenPort, nil)
	}
	if err != nil {
		log.Fatal("tcp2ws Server Start Error: ", err)
	}
}

// 又造轮子了 发现给v4的ip加个框也能连诶
func Tcping(hostname, port string) (int64) {
	st := time.Now().UnixNano()
	c, err := net.DialTimeout("tcp", "[" + hostname + "]" + port, 5 * time.Second)
	if err != nil {
		return -1
	}
	c.Close()
	return (time.Now().UnixNano() - st)/1e6
}

func main() {
	arg_num:=len(os.Args)
	if arg_num < 3 {
		fmt.Println("Client: ws://tcp2wsUrl localPort\nServer: ip:port tcp2wsPort\nUse wss: ip:port tcp2wsPort server.crt server.key")
		fmt.Println("Make ssl cert:\nopenssl genrsa -out server.key 2048\nopenssl ecparam -genkey -name secp384r1 -out server.key\nopenssl req -new -x509 -sha256 -key server.key -out server.crt -days 36500")
		os.Exit(0)
	}
	serverUrl := os.Args[1]
	listenPort := os.Args[2]
	isSsl := false
	if arg_num == 4 {
		isSsl = os.Args[3] == "wss" || os.Args[3] == "https" || os.Args[3] == "ssl"
	}
	sslCrt := "server.crt"
	sslKey := "server.key"
	if arg_num == 5 {
		isSsl = true
		sslCrt = os.Args[3]
		sslKey = os.Args[4]
	}

	// 第一个参数是ws
	match, _ := regexp.MatchString(`^(ws|wss|http|https)://.*`, serverUrl)
	isServer = !match
	if isServer {
		// 服务端
		match, _ := regexp.MatchString(`^\d+$`, serverUrl)
		if match {
			// 只有端口号默认127.0.0.1
			tcpAddr = "127.0.0.1:" + serverUrl
		} else {
			tcpAddr = serverUrl
		}
		// ws server
		http.HandleFunc("/", wsHandler)
		match, _ = regexp.MatchString(`^\d+$`, listenPort)
		listenHostPort := listenPort
		if match {
			// 如果没指定监听ip那就全部监听 省掉不必要的防火墙
			listenHostPort = "0.0.0.0:" + listenPort
		}
		go startWsServer(listenHostPort, isSsl, sslCrt, sslKey)
		if isSsl {
			log.Print("Server Started wss://" +  listenHostPort + " -> " + tcpAddr )
			fmt.Print("Proxy with Nginx:\nlocation /ws/ {\nproxy_pass https://")
		} else {
			log.Print("Server Started ws://" +  listenHostPort + " -> " + tcpAddr )
			fmt.Print("Proxy with Nginx:\nlocation /ws/ {\nproxy_pass http://")
		}
		if match {
			fmt.Print("127.0.0.1:" + listenPort)
		} else {
			fmt.Print(listenPort)
		}
		fmt.Println("/;\nproxy_read_timeout 3600;\nproxy_http_version 1.1;\nproxy_set_header Upgrade $http_upgrade;\nproxy_set_header Connection \"Upgrade\";\nproxy_set_header X-Forwarded-For $remote_addr;\naccess_log off;\n}")
	} else {
		// 客户端
		if serverUrl[:5] == "https"{
			wsAddr = "wss" + serverUrl[5:]
		} else if serverUrl[:4] == "http" {
			wsAddr = "ws" + serverUrl[4:]
		} else {
			wsAddr = serverUrl
		}
		match, _ = regexp.MatchString(`^\d+$`, listenPort)
		listenHostPort := listenPort
		if match {
			// 如果没指定监听ip那就全部监听 省掉不必要的防火墙
			listenHostPort = "0.0.0.0:" + listenPort
		}
		l, err := net.Listen("tcp", listenHostPort)
		if err != nil {
			log.Fatal("tcp2ws Client Start Error: ", err)
		}
		// 将ws服务端域名对应的ip缓存起来，避免多次请求dns或dns爆炸导致无法连接
		u, err := url.Parse(wsAddr)
		if err != nil {
			log.Fatal("tcp2ws Client Start Error: ", err)
		}
		// 确定端口号，下面域名tcping要用
		if u.Port() != "" {
			wsAddrPort = ":" + u.Port()
		} else if wsAddr[:3] == "wss" {
			wsAddrPort = ":443"
		} else {
			wsAddrPort = ":80"
		}
		if u.Host[0] == '[' {
			// ipv6
			wsAddrIp = "[" + u.Hostname() + "]"
			log.Print("tcping " + u.Hostname() + " ", Tcping(u.Hostname(), wsAddrPort), "ms")
		} else if match, _ = regexp.MatchString(`^\d+.\d+.\d+.\d+$`, u.Hostname()); match {
			// ipv4
			wsAddrIp = u.Hostname()
			log.Print("tcping " + wsAddrIp + " ", Tcping(wsAddrIp, wsAddrPort), "ms")
		} else {
			// 域名，需要解析，ip优选
			log.Print("nslookup " + u.Hostname())
			ns, err := net.LookupHost(u.Hostname())
			if err != nil {
				log.Fatal("tcp2ws Client Start Error: ", err)
			}
			wsAddrIp = ns[0]
			var lastPing int64 = 5000 
			for _, n := range ns {
				nowPing := Tcping(n, wsAddrPort)
				log.Print("tcping " + n + " ", nowPing, "ms")
				if nowPing != -1 && nowPing < lastPing {
					wsAddrIp = n
					lastPing = nowPing
				}
			}

			log.Print("Use IP " + wsAddrIp + " for " + u.Hostname())
		}

		go tcpHandler(l)
		log.Print("Client Started " +  listenHostPort + " -> " + wsAddr)
	}
	for {
		if isServer {
			// 心跳间隔2分钟
			time.Sleep(2 * 60 * time.Second)
			nowTimeCut := time.Now().Unix() - 2 * 60
			// check ws
			for k, i := range connMap {
				// 如果超过2分钟没有收到消息，才发心跳，避免读写冲突
				if i.t < nowTimeCut {
					if err := i.wsConn.WriteMessage(websocket.TextMessage, []byte("tcp2wsSparkle"));err != nil{
						log.Print(i.uuid, " timeout close")
						i.tcpConn.Close()
						i.wsConn.Close()
						deleteConn(k)
					}
				}
			}
		} else {
			// 按 ctrl + c 退出，会阻塞 
			c := make(chan os.Signal, 1)
			signal.Notify(c, os.Interrupt, os.Kill)
			<-c
			fmt.Println()
    		log.Print("quit...")
			for k, _ := range connMap {
				deleteConn(k)
			}
			os.Exit(0)
		}
	}
}
