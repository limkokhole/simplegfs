package simplegfs

import (
  "bufio" // For reading lines from a file
  "github.com/wweiw/simplegfs/master"
  log "github.com/Sirupsen/logrus"
  "net"
  "net/rpc"
  "os" // For os file operations
  "strconv" // For string conversion to/from basic data types
  "strings" // For parsing serverMeta
  "sync"
  "time"
)

type MasterServer struct {
  dead bool
  l net.Listener
  me string // Server address
  clientId uint64 // Client ID
  chunkhandle uint64 // Chunkhandle ID
  mutex sync.RWMutex

  // Filename of a file that contains MasterServer metadata
  serverMeta string

  chunkservers map[string]time.Time

  // Namespace manager
  namespaceManager *master.NamespaceManager

  // Chunk manager
  chunkManager *master.ChunkManager
}

// Used for master's mapping from chunkhandle to locations and lease.
type locationsAndLease struct {
  primary string // Selected by master, holds and renews lease on the chunk.
  replicas []string // Chunkservers' addresses that store the chunk.
  leaseEnds time.Time // Time of when the lease expires.
}

// RPC call handlers declared here

// Heartbeat RPC handler for interactions between master
// and chunkservers.
func (ms *MasterServer) Heartbeat(args *HeartbeatArgs,
                                  reply *HeartbeatReply) error {
  ms.chunkservers[args.Addr] = time.Now()
  if len(args.PendingExtensions) > 0 {
    ms.csExtendLease(args.Addr, args.PendingExtensions)
  }
  reply.Reply = "Hello, world."
  return nil
}

// When a new client is attached to the master,
// it calls NewClientId to get a unique ID.
func (ms *MasterServer) NewClientId(args *struct{},
                                    reply *NewClientIdReply) error {
  ms.mutex.Lock()
  defer ms.mutex.Unlock()
  reply.ClientId = ms.clientId
  ms.clientId++
  storeServerMeta(ms)
  return nil
}

// Client calls Create to create a file in the namespace.
func (ms *MasterServer) Create(args string,
                               reply *bool) error {
  // TODO: error handling
  ok, err := ms.namespaceManager.Create(args)
  if !ok {
    *reply = false
    return err
  }
  *reply = true
  return nil
}

// Client calls Mkdir to make a new directory.
func (ms *MasterServer) Mkdir(args string,
                              reply *bool) error {
  ok, err := ms.namespaceManager.Mkdir(args)
  if !ok {
    *reply = false
    return err
  }
  *reply = true
  return nil
}

// List all files or directories under a specific directory.
// Returns empty []string when the argument is not a directory
// or it contains no files and directories.
func (ms *MasterServer) List(args string,
                             reply *ListReply) error {
  paths, err := ms.namespaceManager.List(args)
  reply.Paths = paths
  return err
}

// Delete a file or directory.
// This operation will succeeds only if it is a valid path and
// it contains no children.
func (ms *MasterServer) Delete(args string,
                               reply *bool) error {
  ok, err := ms.namespaceManager.Delete(args)
  *reply = ok
  return err
}

// FindLocations responds to client RPC to get the chunk clocations given file
// name and chunk index. The FindLocationsReply contains a chunkhandle, an
// array of chunk locations.
func (ms *MasterServer) FindLocations(args FindLocationsArgs,
                                      reply *FindLocationsReply) error {
  log.Debugln("Find Locations RPC")
  path := args.Path
  chunkIndex := args.ChunkIndex
  info, err := ms.chunkManager.FindLocations(path, chunkIndex)
  if err != nil {
    return err
  }
  // Set reply message.
  reply.ChunkHandle = info.Handle
  reply.ChunkLocations = info.Locations
  return nil
}

// FindLeaseHolder replies to client RPC to get the primary chunkserver for a
// given chunkhandle. If there is no current lease holder, chunkManager will
// automatically select one of the replicas to be the primary, and grant lease
// to that primary.
func (ms *MasterServer) FindLeaseHolder(args FindLeaseHolderArgs,
                                        reply *FindLeaseHolderReply) error {
  log.Debugln("MasterServer: FindLeaseHolder RPC")
  lease, err := ms.chunkManager.FindLeaseHolder(args.ChunkHandle)
  if err != nil {
    return err
  }
  reply.Primary = lease.Primary
  reply.LeaseEnds = lease.Expiration
  return nil
}

// Client calls AddChunk to get a new chunk.
func (ms *MasterServer) AddChunk(args AddChunkArgs,
                                 reply *AddChunkReply) error {
  log.Debugln(ms.me + " Add chunk RPC")
  path := args.Path
  chunkIndex := args.ChunkIndex
  info, err := ms.chunkManager.AddChunk(path, chunkIndex)
  if err != nil {
    return err
  }
  reply.ChunkHandle = info.Handle
  reply.ChunkLocations = info.Locations
  return nil
}

// Chunk server calls ReportChunk to tell the master
// they have a certain chunk and the number of defined bytes in
// the chunk.
func (ms *MasterServer) ReportChunk(args ReportChunkArgs,
                                    reply *ReportChunkReply) error {
  log.Debugln("MasterServer: Report Chunk.")
  length := args.Length
  handle := args.ChunkHandle
  server := args.ServerAddress
  pathIndex, err := ms.chunkManager.GetPathIndexFromHandle(handle)
  if err != nil {
    return err
  }
  ms.chunkManager.SetChunkLocation(handle, server)
  // Update file information
  fileLength, err := ms.namespaceManager.GetFileLength(pathIndex.Path)
  if err != nil {
    return err
  }
  calculated := int64(ChunkSize * pathIndex.Index) + length
  log.Debugln("Result", calculated, "index", pathIndex.Index, "length", length)
  if calculated > fileLength {
    ms.namespaceManager.SetFileLength(pathIndex.Path, calculated)
    log.Debugln("#### New length:", calculated)
  }
  return nil
}

func (ms *MasterServer) GetFileLength(args string, reply *int64) error {
  log.Debugln("MasterServer: GetFileLength")
  length, err := ms.namespaceManager.GetFileLength(args)
  if err != nil {
    return err
  }
  *reply = length
  return nil
}

// Tell the server to shut itself down
// for testing
func (ms *MasterServer) Kill() {
  ms.dead = true
  ms.l.Close()
}

func StartMasterServer(me string, servers []string) *MasterServer {
  ms := &MasterServer{
    me: me,
    serverMeta: "serverMeta" + me,
    clientId: 1,
    chunkhandle: 1,
    chunkservers: make(map[string]time.Time),
    namespaceManager: master.NewNamespaceManager(),
    chunkManager: master.NewChunkManager(servers),
  }

  loadServerMeta(ms)

  rpcs := rpc.NewServer()
  rpcs.Register(ms)

  l, e := net.Listen("tcp", ms.me)
  if e != nil {
    log.Fatal("listen error", e)
  }
  ms.l = l

  // RPC handler
  go func() {
    for ms.dead == false {
      conn, err := ms.l.Accept()
      if err == nil && ms.dead == false {
        go rpcs.ServeConn(conn)
      } else if err == nil {
        conn.Close()
      } else if err != nil && ms.dead == false {
        log.Debugln("Kill server.")
        ms.Kill()
      }
    }
  }()

  // Background tasks
  go func() {
    for ms.dead == false {
      ms.tick()
      time.Sleep(HeartbeatInterval)
    }
  }()

  return ms
}


// Helper functions

// tick() is called once per PingInterval to
// handle background tasks
func (ms *MasterServer) tick() {
  // TODO: Scan in-memory data structures to find dead chunk servers
}

// storeServerMeta stores master server's meta data persistently.
func storeServerMeta(ms *MasterServer) {
  f, er := os.OpenFile(ms.serverMeta, os.O_RDWR|os.O_CREATE, FilePermRW)
  if er != nil {
    log.Debugln("Open/Create file ", ms.serverMeta, " failed.")
  }
  defer f.Close()

  // Write out clientId
  storeClientId(ms, f)
}

// storeClientId stores MasterServer.clientId on to MasterServer.serverMeta.
func storeClientId(ms *MasterServer, f *os.File) {
  n, err := f.WriteString("clientId " +
                          strconv.FormatUint(ms.clientId, 10) + "\n")
  if err != nil {
    log.Debugln(err)
  } else {
    log.Debugf("Wrote %d bytes to serverMeta\n", n)
  }
}

// loadServerMeta loads MasterServer.serverMeta from disk to instantiate
// master server's meta data.
func loadServerMeta(ms *MasterServer) {
  f, err := os.OpenFile(ms.serverMeta, os.O_RDONLY, FilePermRW)
  if err != nil {
    log.Debugln("Open file ", ms.serverMeta, " failed.");
  }
  defer f.Close()
  parseServerMeta(ms, f)
}

// parseServerMeta parses each line in MasterServer.serverMeta file and loads
// each value into its corresponding MasterServer instance fields.
func parseServerMeta(ms *MasterServer, f *os.File) {
  scanner := bufio.NewScanner(f)
  for scanner.Scan() {
    fields := strings.Fields(scanner.Text())
    switch fields[0] {
    case "clientId":
      var err error
      ms.clientId, err = strconv.ParseUint(fields[1], 0, 64)
      if err != nil {
        log.Fatal("Failed to load clientId into ms.clientId")
      }
    default:
      log.Fatal("Unknown serverMeta key: " + fields[0])
    }
  }
}

// csExtendLease is called by the Heartbeat RPC handler, when chunkservers
// hearbeat message includes lease extension requests. Lease extensions are
// only granted when the requesting chunk server is the current lease holder.
// This function acquires MasterServer.mutex.
func (ms *MasterServer) csExtendLease(cs string, chunks []uint64) {
  ms.chunkManager.ExtendLease(cs, chunks)
}
