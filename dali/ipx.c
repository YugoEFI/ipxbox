
#include <dos.h>
#include <i86.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "inlines.h"
#include "ipx.h"
#include "dbipx.h"

#define TIMER_INTERRUPT       0x08
#define IPX_INTERRUPT         0x7a
#define REDIRECTOR_INTERRUPT  0x2f

#define MAX_OPEN_SOCKETS 8

#define IPX_CMD_OPEN_SOCKET   0x0000
#define IPX_CMD_CLOSE_SOCKET  0x0001
#define IPX_CMD_GET_LOCAL_TGT 0x0002
#define IPX_CMD_SEND_PACKET   0x0003
#define IPX_CMD_LISTEN_PACKET 0x0004
#define IPX_CMD_SCHED_EVENT   0x0005
#define IPX_CMD_CANCEL_OP     0x0006
#define IPX_CMD_SCHED_SPEC    0x0007
#define IPX_CMD_GET_INTERVAL  0x0008
#define IPX_CMD_GET_ADDRESS   0x0009
#define IPX_CMD_RELINQUISH    0x000a
#define IPX_CMD_DISCONNECT    0x000b
#define IPX_CMD_GET_PKT_SIZE  0x000d
#define IPX_CMD_SPX_INSTALLED 0x0010
#define IPX_CMD_GET_MTU       0x001a

#define MTU 1500

struct ipx_socket {
	unsigned short socket;
	struct ipx_ecb far *ecbs;
};

static uint8_t sendbuf[MTU];
static void (__interrupt far *old_isr)(void);
static void (__interrupt far *old_timer_isr)(void);
static void (__interrupt far *next_redirector)(void);
static struct ipx_socket open_sockets[MAX_OPEN_SOCKETS];
static unsigned int num_open_sockets;
static unsigned int saved_ss, saved_sp;

extern void SwitchStack(unsigned int);
#pragma aux SwitchStack = \
	"mov saved_ss, ss" \
	"mov saved_sp, sp" \
	"mov bx, ds" \
	"mov ss, bx" \
	"mov sp, ax" \
	parm [ax] \
	modify [bx];
extern void RestoreStack(void);
#pragma aux RestoreStack =\
	"mov ss, saved_ss" \
	"mov sp, saved_sp";

static unsigned char isr_stack_space[512];
#define SWITCH_ISR_STACK \
	SwitchStack(FP_OFF(isr_stack_space + sizeof(isr_stack_space) - 32))

static struct ipx_socket *FindSocket(unsigned short num)
{
	int i;

	for (i = 0; i < MAX_OPEN_SOCKETS; ++i) {
		if (open_sockets[i].socket == num) {
			return &open_sockets[i];
		}
	}

	return NULL;
}

static size_t ECBSize(struct ipx_ecb far *ecb)
{
	size_t result = 0;
	int i;

	for (i = 0; i < ecb->fragment_count; ++i) {
		result += ecb->fragments[i].size;
	}

	return result;
}

static struct ipx_ecb far * far *FindECB(struct ipx_socket *sock, size_t len)
{
	struct ipx_ecb far * far *ecb;

	ecb = &sock->ecbs;
	while (*ecb != NULL) {
		if (ECBSize(*ecb) >= len) {
			return ecb;
		}
		ecb = &(*ecb)->next_ecb;
	}

	return NULL;
}

static void FillECB(struct ipx_ecb far *ecb, const uint8_t *data, size_t len)
{
	uint8_t far *fragptr;
	size_t nbytes;
	int i;
	for (i = 0; i < ecb->fragment_count; ++i) {
		nbytes = ecb->fragments[i].size;
		if (nbytes > len) {
			nbytes = len;
		}
		fragptr = MK_FP(ecb->fragments[i].seg, ecb->fragments[i].off);
		_fmemcpy(fragptr, data, nbytes);
		data += nbytes;
		len -= nbytes;
	}
}

static void PacketReceived(const struct ipx_header *pkt, size_t len)
{
	struct ipx_socket *sock;
	struct ipx_ecb far * far *ecb;

	if (pkt->dest.socket == 0) {
		return;
	}

	sock = FindSocket(ntohs(pkt->dest.socket));
	if (sock == NULL) {
		return;
	}
	ecb = FindECB(sock, len);
	if (ecb == NULL) {
		return;
	}
	FillECB(*ecb, (const uint8_t *) pkt, len);

	// Mark as delivered and unhook from linked list.
	_fmemcpy(&(*ecb)->immediate_address, pkt->src.node, 6);
	(*ecb)->in_use = 0;
	(*ecb)->completion_code = 0;
	// TODO: ESR notification
	*ecb = (*ecb)->next_ecb;
}

static void OpenSocket(union INTPACK far *ip)
{
	struct ipx_socket *sock;
	int socknum;

	socknum = ntohs(ip->w.dx);

	if (socknum == 0) {
		socknum = 0x4002;
		while (FindSocket(socknum) != NULL) {
			++socknum;
		}
	}

	// Already in use?
	if (FindSocket(socknum) != NULL) {
		ip->w.ax = 0xff;
		return;
	}

	sock = FindSocket(0);
	if (sock == NULL) {
		ip->w.ax = 0xfe;
		return;
	}

	sock->socket = socknum;
	sock->ecbs = NULL;
	ip->w.ax = 0;
	ip->w.dx = htons(socknum);
}

static void CloseSocket(unsigned int num)
{
	struct ipx_socket *sock;

	if (num == 0) {
		return;
	}

	sock = FindSocket(num);
	if (sock == NULL) {
		return;
	}

	sock->socket = 0;
}

static int SendPacket(struct ipx_ecb far *ecb)
{
	struct ipx_header *pkt;
	uint8_t far *fragptr;
	int size;
	int i;

	size = 0;
	for (i = 0; i < ecb->fragment_count; ++i) {
		size += ecb->fragments[i].size;
	}

	if (size > MTU) {
		ecb->in_use = 0;
		ecb->completion_code = 0xff;
		return 0xff;
	}

	size = 0;
	for (i = 0; i < ecb->fragment_count; ++i) {
		fragptr = MK_FP(ecb->fragments[i].seg, ecb->fragments[i].off);
		_fmemcpy(&sendbuf[size], fragptr, ecb->fragments[i].size);
		size += ecb->fragments[i].size;
	}

	pkt = (struct ipx_header *) sendbuf;
	_fmemcpy(&pkt->src, &dbipx_local_addr, sizeof(struct ipx_address));
	pkt->src.socket = ecb->socket;
	pkt->length = ntohs(size);

	// TODO: Copy back modified header into fragment

	DBIPX_SendPacket(pkt, size);

	// TODO: Loopback delivery and broadcast

	ecb->in_use = 0;
	ecb->completion_code = 0;
	// TODO: ESR notification

	return 0;
}

static int ListenPacket(struct ipx_ecb far *ecb)
{
	struct ipx_socket *sock = FindSocket(ntohs(ecb->socket));

	if (sock == NULL) {
		ecb->completion_code = 0xff;
		return 0xff;
	}

	ecb->next_ecb = sock->ecbs;
	sock->ecbs = ecb;
	ecb->in_use = 1;
	return 0;
}

static void GetLocalTarget(uint8_t far *dest, struct ipx_address far *src)
{
	_fmemcpy(dest, src->node, 6);
}

static void Real_IPX_ISR(union INTPACK far *ip)
{
	switch (ip->w.bx) {
		case IPX_CMD_OPEN_SOCKET:
			OpenSocket(ip);
			break;
		case IPX_CMD_CLOSE_SOCKET:
			CloseSocket(ntohs(ip->w.dx));
			break;
		case IPX_CMD_GET_LOCAL_TGT:
			GetLocalTarget(MK_FP(ip->w.es, ip->w.di),
			               MK_FP(ip->w.es, ip->w.si));
			ip->w.ax = 0;
			break;
		case IPX_CMD_SEND_PACKET:
			ip->w.ax = SendPacket(MK_FP(ip->w.es, ip->w.si));
			break;
		case IPX_CMD_LISTEN_PACKET:
			ip->w.ax = ListenPacket(MK_FP(ip->w.es, ip->w.si));
			break;
		case IPX_CMD_SCHED_EVENT:
			// TODO
			break;
		case IPX_CMD_CANCEL_OP:
			// TODO
			break;
		case IPX_CMD_SCHED_SPEC:
			// TODO
			break;
		case IPX_CMD_GET_INTERVAL:
			// TODO
			break;
		case IPX_CMD_GET_ADDRESS:
			_fmemcpy(MK_FP(ip->w.es, ip->w.si),
			         &dbipx_local_addr, sizeof(struct ipx_address));
			break;
		case IPX_CMD_RELINQUISH:
		case IPX_CMD_DISCONNECT:
			// no-op
			break;
		case IPX_CMD_GET_PKT_SIZE:
			ip->w.ax = 1024;
			ip->w.cx = 0;
			break;
		case IPX_CMD_SPX_INSTALLED:
			ip->w.ax = 0;
			break;
		case IPX_CMD_GET_MTU:
			ip->w.ax = MTU;
			ip->w.cx = 0;
			break;
		default:
			break;
	}
}

static void __interrupt __far IPX_ISR(union INTPACK ip)
{
	static union INTPACK far *_ip;
	_ip = &ip;

	SWITCH_ISR_STACK;
	Real_IPX_ISR(_ip);
	RestoreStack();
}

extern void IPXTrampolineASM();
#pragma aux IPXTrampolineASM = \
	"int 0x7a"

static void __far IPXTrampoline(void)
{
	// For the trampoline function we just invoke the actual interrupt.
	// This is the laziest possible implementation of this function.
	IPXTrampolineASM();
}

static void __interrupt __far RedirectorISR(union INTPACK ip)
{
	// There are two entrypoints to the IPX API. One is the 0x7a interrupt,
	// and the other is via the redirector which sets es:di to the
	// location of a routine to jump to.
	if (ip.w.ax == 0x7a00) {
		ip.h.al = 0xff;
		ip.w.es = FP_SEG(IPXTrampoline);
		ip.w.di = FP_OFF(IPXTrampoline);
		return;
	}

	// TODO: Answer IPX API requests on this ISR too?

	_chain_intr(next_redirector);
}

static void __interrupt __far TimerISR(union INTPACK ip)
{
	SWITCH_ISR_STACK;
	DBIPX_Poll();
	RestoreStack();

	_chain_intr(old_timer_isr);
}

static void UnhookVector(void)
{
	_disable();
	DBIPX_SetCallback(NULL);
	_dos_setvect(IPX_INTERRUPT, old_isr);
	_dos_setvect(REDIRECTOR_INTERRUPT, next_redirector);
	_dos_setvect(TIMER_INTERRUPT, old_timer_isr);
	_enable();
}

void HookIPXVector(void)
{
	_disable();
	old_isr = _dos_getvect(IPX_INTERRUPT);
	_dos_setvect(IPX_INTERRUPT, IPX_ISR);
	next_redirector = _dos_getvect(REDIRECTOR_INTERRUPT);
	_dos_setvect(REDIRECTOR_INTERRUPT, RedirectorISR);
	old_timer_isr = _dos_getvect(TIMER_INTERRUPT);
	_dos_setvect(TIMER_INTERRUPT, TimerISR);
	DBIPX_SetCallback(PacketReceived);
	_enable();

	atexit(UnhookVector);
}

