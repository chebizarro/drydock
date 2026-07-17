import { describe, expect, it } from 'vitest';
import { finalizeEvent, getPublicKey, type Event } from 'nostr-tools';
import {
    buildSessionAnnouncementTags,
    hasResponseCorrelationTags,
    isTrustedGatewayEvent,
    type ResponseCorrelation
} from './protocol';

const gatewayKey = privateKey(1);
const gatewayPubkey = getPublicKey(gatewayKey);
const clientPubkey = getPublicKey(privateKey(2));

const correlation: ResponseCorrelation = {
    clientPubkey,
    requestEventId: 'a'.repeat(64),
    sessionId: 'session-1',
    requestId: 'request-1',
    fixId: 'fix-1'
};

describe('IDE Nostr protocol validation', () => {
    it('includes the configured gateway p tag in session announcements', () => {
        const tags = buildSessionAnnouncementTags('session-1', '0.1.0', gatewayPubkey);

        expect(tags).toContainEqual(['p', gatewayPubkey]);
    });

    it('rejects forged or unsigned gateway events', () => {
        const signed = responseEvent(correlation);
        const forged = { ...signed, content: '{"forged":true}' } as Event;
        const unsigned = {
            ...signed,
            id: '0'.repeat(64),
            sig: '0'.repeat(128)
        } as Event;

        expect(isTrustedGatewayEvent(forged, gatewayPubkey)).toBe(false);
        expect(isTrustedGatewayEvent(unsigned, gatewayPubkey)).toBe(false);
    });

    it.each([
        ['p', { clientPubkey: getPublicKey(privateKey(3)) }],
        ['e', { requestEventId: 'b'.repeat(64) }],
        ['session', { sessionId: 'session-2' }],
        ['request', { requestId: 'request-2' }],
        ['fix', { fixId: 'fix-2' }]
    ])('rejects a mismatched %s correlation tag', (_tag, mismatch) => {
        const event = responseEvent(correlation);

        expect(hasResponseCorrelationTags(event, { ...correlation, ...mismatch })).toBe(false);
    });
});

function responseEvent(expected: ResponseCorrelation): Event {
    return finalizeEvent({
        kind: 25910,
        created_at: 1_700_000_000,
        content: '{}',
        tags: [
            ['p', expected.clientPubkey],
            ['e', expected.requestEventId],
            ['session', expected.sessionId],
            ['request', expected.requestId],
            ['fix', expected.fixId!]
        ]
    }, gatewayKey);
}

function privateKey(lastByte: number): Uint8Array {
    const key = new Uint8Array(32);
    key[31] = lastByte;
    return key;
}
