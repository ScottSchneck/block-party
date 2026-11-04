# Console server list: proof-of-concept notes

The console server list (`internal/serverlist`) started as a stand-alone proof
of concept, tested on a Nintendo Switch (Minecraft 1.26.50) in September 2026
across four builds. This page records what was learned, so the reasons behind
the defaults aren't lost.

## How it works

1. The console looks up a featured-server name such as `geo.hivebedrock.network`.
2. The built-in DNS answers with the panel's address.
3. The console joins the panel, which spawns it into an empty world and shows
   a menu of your servers.
4. The player picks one and the panel sends a Transfer packet with that
   server's address and port.

## Results

| Question | Result |
| --- | --- |
| Does the built-in DNS catch the featured names? | Yes. The Switch looks them all up when Minecraft opens |
| Is Xbox sign-in verified? | Yes: gamertag and XUID arrive signed |
| Do the menu and transfer work? | Yes, including transfers to NetherNet game servers |
| Is there a trust prompt? | Yes, once per game server, from the NetherNet game server itself, not from the list |
| Can the redirect use NetherNet? | No. The Switch rejects the HTTPS certificate for another company's domain (a valid one is impossible to get). It pings over plain HTTP but always joins over RakNet. Offering NetherNet on a name doesn't break anything: the console falls back to RakNet |
| How long until the menu appears? | About 19 s on the Switch, against 32 s for the public BedrockConnect server. The handshake finishes in about 2 s; the rest is the Switch loading its world |

### PlayStation 5 (build 5)

The PS5 (Minecraft 1.26.52) reached the menu in 5.6 s and was transferred to
the game server. Its sign-in is different from the Switch's:

- It sends only a token, with no Mojang certificate chain.
- The token is signed with the console's own key (ES384, `authType=2`), not
  by Microsoft. It still carries an XUID and gamertag, plus platform claims
  (`pid`, `pname`, `nid`, `nname`, `ap`).
- gophertunnel v1.62.0 rejected it outright ("unexpected signature algorithm
  ES384"), so build 4 dropped the PS5 before the menu.

Build 5 no longer turns consoles away over sign-in. It checks and logs the
result, and leaves the real check to the game server, which admitted the PS5.
The consequence for the design: **the server list can't trust a PS5's XUID or
gamertag**, so anything that restricts who sees the menu (for example in
friends mode) can't rely on it. Allowlists stay on the game servers.

**LAN Games:** on the home network the PS5 also found the list by itself under
Friends → LAN Games ("Server list / Pick a server"), with no DNS change. The
list answers the consoles' LAN search on port 19132. The name shown is now set
with `LIST_NAME` and `LIST_SUBTITLE`.

After changing a console's DNS, restart the console, or it keeps using cached
answers.

## Why these defaults

- **`CONNECTION=both`** (since 1.1.0): Minecraft 26.60 (October 27, 2026)
  stops joining servers over RakNet, so the list takes NetherNet as well.
  Consoles still find it under LAN Games with a RakNet search on 19132.
  `NETHERNET_NAMES` can limit NetherNet to some featured-server names, so it
  can be re-tested after Minecraft updates without a new build. If consoles ever drop RakNet for featured servers, the redirect
  stops working for every tool, BedrockConnect included. The design's fallback
  is LAN broadcast and Friends-tab broadcast, which don't depend on it.
- **Terrain goes out right after the view distance is agreed.** In build 1 the
  terrain was sent after the console reported it had spawned. The console
  waits for terrain before it finishes spawning, so both sides waited until
  it timed out (about 18 s).
- **`LOAD_PRESET=bedrockconnect`** copies BedrockConnect's empty world
  (survival at height 0, terrain with StartGame, its chunk format). It made no
  difference on the Switch and is kept only for comparison testing.

## The spawn race in gophertunnel

Testing with the fake console showed about 1 join in 10 stalling over
loopback. The cause is a timing gap in gophertunnel v1.62.0:
`Conn.StartGame` sends StartGame and flushes it, and only then marks the
client's view-distance request (RequestChunkRadius) as expected. A client
that answers inside that gap has its request set aside for `ReadPacket`
instead of handled, and the join waits until it times out.

`rescueViewDistance` in `session.go` works around it. It watches for the
request; if the library hasn't answered within 500 ms, it takes the set-aside
request with `ReadPacket` and answers it the way the library would. With it,
120 of 120 fake-console joins passed, with 6 rescued. Real consoles answer
more slowly than loopback, so they rarely hit the gap, but a busy host can
widen it.

gophertunnel 1.63 fixed it (it now expects the client's replies before
sending StartGame), so the workaround was removed in Block Party 1.4.3.
