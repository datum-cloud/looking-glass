#!/usr/bin/env bash
# Records the FRR output the frr driver's tests replay, by running the
# driver's exact command lines on r1. Run it from this directory with Docker:
#
#   ./record.sh .
#
# Recorded with FRR 10.7.1, iputils ping 20240905 and traceroute 2.1.6.
#
# It builds three FRR routers and one host on three isolated Docker networks,
# with documentation and private addresses only:
#   r1  AS64496, the router under test
#   r2  AS64497, an external peer; announces 198.51.100.0/24, 2001:db8:100::/48
#   r3  AS64496, an internal peer; announces 203.0.113.0/24, 2001:db8:300::/48
#   r5  a host behind r2 holding 198.51.100.3 and 2001:db8:100::3
# Set KEEP=1 to leave the containers running after recording.
set -euo pipefail
IMG=${FRR_IMAGE:-quay.io/frrouting/frr:10.7.1}
OUT=${1:?usage: record.sh <outdir>}
mkdir -p "$OUT"
cleanup() { docker rm -f lg40-r1 lg40-r2 lg40-r3 lg40-r5 >/dev/null 2>&1 || true; docker network rm lg40-ext lg40-int lg40-far >/dev/null 2>&1 || true; }
[ -n "${KEEP:-}" ] || trap cleanup EXIT
cleanup
docker network create --ipv6 -o com.docker.network.bridge.gateway_mode_ipv4=nat-unprotected -o com.docker.network.bridge.gateway_mode_ipv6=nat-unprotected --subnet 192.0.2.0/24 --subnet 2001:db8:2::/64 lg40-ext >/dev/null
docker network create --ipv6 -o com.docker.network.bridge.gateway_mode_ipv4=nat-unprotected -o com.docker.network.bridge.gateway_mode_ipv6=nat-unprotected --subnet 10.240.40.0/24 --subnet fd00:40::/64 lg40-int >/dev/null
docker network create --ipv6 -o com.docker.network.bridge.gateway_mode_ipv4=nat-unprotected -o com.docker.network.bridge.gateway_mode_ipv6=nat-unprotected --subnet 10.240.41.0/24 --subnet fd00:41::/64 lg40-far >/dev/null
for r in r1 r2 r3 r5; do
  docker run -d --name lg40-$r --hostname $r --privileged --network none --sysctl net.ipv6.conf.all.disable_ipv6=0 \
    --sysctl net.ipv4.ip_forward=1 --sysctl net.ipv6.conf.all.forwarding=1 --entrypoint sleep "$IMG" infinity >/dev/null
done
docker network disconnect none lg40-r1; docker network disconnect none lg40-r2; docker network disconnect none lg40-r3; docker network disconnect none lg40-r5
docker network connect --ip 192.0.2.2 --ip6 2001:db8:2::2 lg40-ext lg40-r1
docker network connect --ip 192.0.2.3 --ip6 2001:db8:2::3 lg40-ext lg40-r2
docker network connect --ip 10.240.40.2 --ip6 fd00:40::2 lg40-int lg40-r1
docker network connect --ip 10.240.40.3 --ip6 fd00:40::3 lg40-int lg40-r3
docker network connect --ip 10.240.41.2 --ip6 fd00:41::2 lg40-far lg40-r2
docker network connect --ip 10.240.41.3 --ip6 fd00:41::3 lg40-far lg40-r5

conf() { docker exec -i lg40-$1 sh -c 'cat > /etc/frr/frr.conf'; }
conf r1 <<'C'
frr defaults traditional
hostname r1
router bgp 64496
 bgp router-id 192.0.2.254
 no bgp ebgp-requires-policy
 neighbor 192.0.2.3 remote-as 64497
 neighbor 192.0.2.3 description transit-a
 neighbor 2001:db8:2::3 remote-as 64497
 neighbor 2001:db8:2::3 description transit-a-v6
 neighbor 10.240.40.3 remote-as 64496
 neighbor 10.240.40.3 description core-r3
 neighbor fd00:40::3 remote-as 64496
 address-family ipv4 unicast
  neighbor 10.240.40.3 next-hop-self
  neighbor 10.240.40.3 send-community all
  neighbor 192.0.2.3 send-community all
 exit-address-family
 address-family ipv6 unicast
  neighbor 2001:db8:2::3 activate
  neighbor fd00:40::3 activate
 exit-address-family
ip route 198.18.0.0/15 Null0
C
conf r2 <<'C'
frr defaults traditional
hostname r2
route-map OUT permit 10
 set community 64497:100 64497:200 65535:666
 set large-community 64497:1:2 64497:3:4
 set metric 50
router bgp 64497
 bgp router-id 192.0.2.3
 no bgp ebgp-requires-policy
 neighbor 192.0.2.2 remote-as 64496
 neighbor 2001:db8:2::2 remote-as 64496
 address-family ipv4 unicast
  network 198.51.100.0/24 route-map OUT
  neighbor 192.0.2.2 send-community all
 exit-address-family
 address-family ipv6 unicast
  network 2001:db8:100::/48 route-map OUT
  neighbor 2001:db8:2::2 activate
  neighbor 2001:db8:2::2 send-community all
 exit-address-family
ip route 198.51.100.0/24 Null0
ipv6 route 2001:db8:100::/48 Null0
C
conf r3 <<'C'
frr defaults traditional
hostname r3
router bgp 64496
 bgp router-id 10.240.40.3
 neighbor 10.240.40.2 remote-as 64496
 neighbor fd00:40::2 remote-as 64496
 address-family ipv4 unicast
  network 203.0.113.0/24
 exit-address-family
 address-family ipv6 unicast
  network 2001:db8:300::/48
  neighbor fd00:40::2 activate
 exit-address-family
ip route 203.0.113.0/24 Null0
ipv6 route 2001:db8:300::/48 Null0
C
for r in r1 r2 r3; do
  docker exec lg40-$r sh -c 'touch /etc/frr/vtysh.conf; sed -i "s/^bgpd=no/bgpd=yes/" /etc/frr/daemons; apk add -q iputils traceroute >/dev/null; /usr/lib/frr/frrinit.sh start >/dev/null 2>&1'
done
# r5 holds the probe targets and routes everything back through r2. Docker
# drops traffic to its own container addresses from other bridges, so the
# targets are loopback addresses Docker does not manage.
docker exec lg40-r5 sh -c 'ip addr add 198.51.100.3/32 dev lo; ip -6 addr add 2001:db8:100::3/128 dev lo; ip route replace default via 10.240.41.2; ip -6 route replace default via fd00:41::2'
docker exec lg40-r2 sh -c 'ip route add 198.51.100.3/32 via 10.240.41.3; ip -6 route add 2001:db8:100::3/128 via fd00:41::3'
for i in $(seq 60); do
  n=$(docker exec lg40-r1 vtysh -c 'show bgp summary json' | grep -c '"state":"Established"' || true)
  [ "$n" -ge 4 ] && break; sleep 1
done
sleep 5

rec() { docker exec lg40-r1 sh -c "$2" > "$OUT/$1" || true; }
rec route-v4-address.json      'vtysh -c "show ip route 198.51.100.7 json"'
rec route-v4-prefix.json       'vtysh -c "show ip route 198.51.100.0/24 json"'
rec route-v4-exact-missing.json 'vtysh -c "show ip route 198.51.100.0/25 json"'
rec route-v4-internal.json     'vtysh -c "show ip route 203.0.113.0/24 json"'
rec route-v4-static.json       'vtysh -c "show ip route 198.18.0.0/15 json"'
rec route-v4-connected.json    'vtysh -c "show ip route 192.0.2.9 json"'
rec route-v6-address.json      'vtysh -c "show ipv6 route 2001:db8:100::7 json"'
rec bgp-v4-prefix.json         'vtysh -c "show bgp ipv4 unicast 198.51.100.0/24 json"'
rec bgp-v4-internal.json       'vtysh -c "show bgp ipv4 unicast 203.0.113.0/24 json"'
rec bgp-v6-prefix.json         'vtysh -c "show bgp ipv6 unicast 2001:db8:100::/48 json"'
rec bgp-v4-summary.json        'vtysh -c "show bgp ipv4 unicast summary json"'
rec bgp-v6-summary.json        'vtysh -c "show bgp ipv6 unicast summary json"'
rec ping-v4.txt       'ping -4 -n -c 3 -W 1 -s 56 -I 192.0.2.2 198.51.100.3'
rec ping-v4-loss.txt  'ping -4 -n -c 3 -W 1 -s 56 -I 192.0.2.2 198.51.100.200'
rec ping-v6.txt       'ping -6 -n -c 3 -W 1 -s 56 -I 2001:db8:2::2 2001:db8:100::3'
rec trace-v4.txt      'traceroute -4 -n -q 3 -m 20 -w 1 -s 192.0.2.2 198.51.100.3 84'
rec trace-v4-loss.txt 'traceroute -4 -n -q 3 -m 4 -w 1 -s 192.0.2.2 198.51.100.200 84'
rec trace-v6.txt      'traceroute -6 -n -q 3 -m 20 -w 1 -s 2001:db8:2::2 2001:db8:100::3 104'
