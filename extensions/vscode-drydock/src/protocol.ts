import { verifyEvent, type Event } from 'nostr-tools';

export interface ResponseCorrelation {
    clientPubkey: string;
    requestEventId: string;
    sessionId: string;
    requestId: string;
    fixId?: string;
}

export function buildSessionAnnouncementTags(
    sessionId: string,
    extensionVersion: string,
    gatewayPubkey: string
): string[][] {
    return [
        ['d', `drydock:ide-session:${sessionId}`],
        ['p', gatewayPubkey],
        ['type', 'ide-session'],
        ['schema', 'drydock.ide-session.v1'],
        ['client', `vscode-drydock/${extensionVersion}`]
    ];
}

export function isTrustedGatewayEvent(event: Event, gatewayPubkey: string): boolean {
    if (gatewayPubkey.length === 0 || event.pubkey !== gatewayPubkey) {
        return false;
    }

    // Verify a fresh event object so a cached nostr-tools verification marker cannot
    // survive mutation by an untrusted caller.
    return verifyEvent({
        kind: event.kind,
        created_at: event.created_at,
        content: event.content,
        tags: event.tags,
        pubkey: event.pubkey,
        id: event.id,
        sig: event.sig
    });
}

export function hasResponseCorrelationTags(event: Event, expected: ResponseCorrelation): boolean {
    return hasTagValue(event, 'p', expected.clientPubkey)
        && hasTagValue(event, 'e', expected.requestEventId)
        && hasTagValue(event, 'session', expected.sessionId)
        && hasTagValue(event, 'request', expected.requestId)
        && (!expected.fixId || hasTagValue(event, 'fix', expected.fixId));
}

function hasTagValue(event: Event, name: string, value: string): boolean {
    return event.tags.some(tag => tag.length >= 2 && tag[0] === name && tag[1] === value);
}
