// unified-testing transport contract, with ChainSafe's native QUIC-v1 transport.
// The stock upstream JS test plan does not enable QUIC; this one deliberately does.
import { createLibp2p } from 'libp2p'
import { quic } from '@chainsafe/libp2p-quic'
import { identify } from '@libp2p/identify'
import { ping } from '@libp2p/ping'
import { multiaddr } from '@multiformats/multiaddr'
import { createClient } from 'redis'

const env = process.env
let node, redis, watchdog
try {
  if (env.TRANSPORT !== 'quic-v1') throw new Error('Only quic-v1 is supported')
  if (!env.TEST_KEY || !env.REDIS_ADDR) throw new Error('TEST_KEY and REDIS_ADDR are required')
  if (!['true', 'false'].includes(env.IS_DIALER)) throw new Error('IS_DIALER must be true or false')
  const seconds = Number(env.TEST_TIMEOUT_SECS ?? '180')
  if (!Number.isInteger(seconds) || seconds < 1 || seconds > 3600) throw new Error('Invalid timeout')
  const signal = AbortSignal.timeout(seconds * 1000)
  // Also bounds Redis waits, startup and shutdown, not just libp2p operations.
  watchdog = setTimeout(() => {
    console.error('Transport test deadline exceeded')
    process.exit(1)
  }, seconds * 1000)
  redis = createClient({ url: `redis://${env.REDIS_ADDR}`, socket: { reconnectStrategy: false } })
  redis.on('error', err => console.error('Redis:', err))
  await redis.connect()
  node = await createLibp2p({
    addresses: { listen: [`/ip4/${env.LISTENER_IP ?? '0.0.0.0'}/udp/0/quic-v1`] },
    transports: [quic({ ipv6: false })],
    services: { identify: identify(), ping: ping() }
  })
  const key = `${env.TEST_KEY}_listener_multiaddr`
  if (env.IS_DIALER === 'true') {
    const item = await redis.blPop(key, seconds)
    if (item === null) throw new Error('Listener address deadline exceeded')
    const addr = multiaddr(item.element)
    console.error('Dialing', addr.toString())
    const start = performance.now()
    await node.dial(addr, { signal })
    const pingRTT = await node.services.ping.ping(addr, { signal })
    const handshake = performance.now() - start
    // js-libp2p ping uses Date.now(): a valid local RTT may round to zero ms.
    if (!Number.isFinite(pingRTT) || pingRTT < 0) throw new Error('Invalid ping RTT')
    console.log(JSON.stringify({ handshakePlusOneRTTMillis: handshake, pingRTTMilllis: pingRTT }))
    console.log(`latency:\n  handshake_plus_one_rtt: ${handshake}\n  ping_rtt: ${pingRTT}\n  unit: ms`)
  } else {
    const addr = node.getMultiaddrs().find(addr => /^\/ip4\/(?!127\.|0\.0\.0\.0\/)/.test(addr.toString()))
    if (!addr) throw new Error('No reachable IPv4 QUIC listen address')
    await redis.rPush(key, addr.toString())
    console.error('Listening', addr.toString())
    // The runner stops the listener after the dialer has produced measurements.
    await new Promise(resolve => {
      process.once('SIGTERM', resolve)
      process.once('SIGINT', resolve)
    })
  }
} catch (err) {
  console.error(err)
  process.exitCode = 1
} finally {
  try {
    await node?.stop()
    if (redis?.isOpen) await redis.close()
  } catch (err) {
    console.error('Shutdown:', err)
    process.exitCode = 1
  }
  clearTimeout(watchdog)
}
