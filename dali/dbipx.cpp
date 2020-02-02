
#include <stdarg.h>
#include <stdlib.h>
#include <string.h>

extern "C" {
#include "ipx.h"
}

#include "dbipx.h"

#include "dns.h"
#include "timer.h"
#include "udp.h"

#define REG_ATTEMPTS 5
#define MTU 576

static uint8_t buf[MTU];
static IpAddr_t server_addr;
static int udp_port;
static int registered;
static struct ipx_address local_addr;

extern "C" {

// Aborts the program with an abnormal program termination.
void Error(char *fmt, ...)
{
	va_list args;

	va_start(args, fmt);
	vfprintf(stderr, fmt, args);
	va_end(args);

	exit(1);
}

static void PacketReceived(const unsigned char *packet, const UdpHeader *udp)
{
	const struct ipx_header *ipx;

	if (udp->len < sizeof(struct ipx_header)) {
		return;
	}
	ipx = (const struct ipx_header *) packet;
	if (ntohs(ipx->src.socket) == 2 && ntohs(ipx->dest.socket) == 2) {
		registered = 1;
		memcpy(&local_addr, &ipx->dest, sizeof(struct ipx_address));
		return;
	}
}

static void SendRegistration(void)
{
	struct ipx_header ipx;

	memset(&ipx, 0, sizeof(ipx));
	ipx.dest.socket = 2;
	ipx.src.socket = 2;
	ipx.checksum = 0xffff;
	ipx.length = 0x1e;
	ipx.transport_control = 0;
	ipx.type = 0xff;

	Udp::sendUdp(server_addr, udp_port, udp_port,
	             sizeof(ipx), (unsigned char *) &ipx, 0);
}

static void Delay(int timer_ticks)
{
	clockTicks_t start = TIMER_GET_CURRENT();

	while (Timer_diff(start, TIMER_GET_CURRENT()) < timer_ticks) {
	}
}

void DBIPX_Connect(const char *addr, int port)
{
	int i;

	udp_port = port;

	if (Dns::resolve(addr, server_addr, 1) < 0) {
		Error("Failed to resolve server address '%s'", addr);
	}

	registered = 0;
	Udp::registerCallback(port, PacketReceived);

	Delay(TIMER_TICKS_PER_SEC);

	for (i = 0; !registered && i < REG_ATTEMPTS*TIMER_TICKS_PER_SEC; ++i) {
		if ((i % TIMER_TICKS_PER_SEC) == 0) {
			SendRegistration();
		}
		Delay(1);
	}

	if (!registered) {
		Error("No response from server at %s:%d", addr, port);
	}
}

}

