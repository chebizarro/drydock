/**
 * Drydock VS Code Extension
 * 
 * Integrates with Drydock code review service via Nostr protocol.
 * 
 * Features:
 * - Review uncommitted changes with AI
 * - Display findings as VS Code diagnostics
 * - One-click fix application
 * - Real-time updates via Nostr subscriptions
 */

import * as vscode from 'vscode';
import { execSync } from 'child_process';
import { v4 as uuidv4 } from 'uuid';
import {
    finalizeEvent,
    getPublicKey,
    nip19,
    SimplePool,
    type Event,
    type EventTemplate,
    type Filter,
    type VerifiedEvent
} from 'nostr-tools';

const { useWebSocketImplementation } = require('nostr-tools/pool') as {
    useWebSocketImplementation: (implementation: unknown) => void;
};
const NodeWebSocket = require('ws');

useWebSocketImplementation(NodeWebSocket);

// Nostr event kinds for IDE integration
const KIND_IDE_SESSION = 30078;
const IDE_SESSION_SCHEMA = 'drydock.ide-session.v1';
const KIND_CONTEXTVM = 25910;
const JSONRPC_VERSION = '2.0';
const METHOD_REVIEW_REQUEST = 'review/request';
const METHOD_APPLY_FIX = 'review/apply-fix';

// Diagnostic collection for displaying findings
let diagnosticCollection: vscode.DiagnosticCollection;

// Session state
let sessionId: string;
let extensionVersion = '0.0.0';

// Relay state
let relayPool: SimplePool | undefined;
let reviewResponseSubscription: { close: (reason?: string) => void | Promise<void> } | undefined;
let activeRelayUrls: string[] = [];
let activeSubscriptionKey: string | undefined;
let latestReviewRequestId: string | undefined;

// Store pending fixes by ID
const pendingFixes: Map<string, PendingFix> = new Map();
const pendingFixRequests: Map<string, { fixId: string; file: string; range: vscode.Range }> = new Map();

interface PendingFix {
    file: string;
    range: vscode.Range;
    suggestedFix?: string;
}

interface Diagnostic {
    file: string;
    range: {
        start_line: number;
        start_column: number;
        end_line: number;
        end_column: number;
    };
    severity: number;
    message: string;
    source: string;
    code?: string;
    has_fix?: boolean;
    suggested_fix?: string;
    fix_id?: string;
}

interface ReviewResponse {
    request_id: string;
    session_id: string;
    diagnostics: Diagnostic[];
    summary: string;
    review_time_ms: number;
}

interface IDESessionAnnouncement {
    session_id: string;
    workspace_path: string;
    repo_id: string;
    editor: string;
    version: string;
    languages: string[];
}

interface ReviewRequest {
    session_id: string;
    request_id: string;
    diff: string;
    changed_files: string[];
    full_review: boolean;
}

interface FixRequest {
    session_id: string;
    request_id: string;
    fix_id: string;
    file: string;
}

interface FixResponse {
    request_id?: string;
    session_id?: string;
    fix_id: string;
    success: boolean;
    patch?: string;
}

interface JSONRPCRequest<T> {
    jsonrpc: '2.0';
    id: string;
    method: string;
    params: T;
}

interface JSONRPCResponse<T> {
    jsonrpc: string;
    id: string;
    result?: T;
    error?: {
        code: number;
        message: string;
        data?: unknown;
    };
}

interface DrydockConfig {
    relays: string[];
    privateKey: string;
    drydockPubkey: string;
}

export function activate(context: vscode.ExtensionContext) {
    console.log('Drydock extension activated');

    extensionVersion = String(context.extension.packageJSON.version ?? '0.0.0');

    // Create diagnostic collection
    diagnosticCollection = vscode.languages.createDiagnosticCollection('drydock');
    context.subscriptions.push(diagnosticCollection);

    // Generate session ID
    sessionId = uuidv4();

    // Register commands
    context.subscriptions.push(
        vscode.commands.registerCommand('drydock.reviewChanges', reviewChanges),
        vscode.commands.registerCommand('drydock.applyFix', applyFix),
        vscode.commands.registerCommand('drydock.clearDiagnostics', clearDiagnostics),
        vscode.workspace.onDidChangeConfiguration(event => {
            if (event.affectsConfiguration('drydock.relays') ||
                event.affectsConfiguration('drydock.privateKey') ||
                event.affectsConfiguration('drydock.drydockPubkey')) {
                void refreshRelaySubscription({ announceSession: false, notifyOnFailure: true });
            }
        }),
        new vscode.Disposable(() => {
            void reviewResponseSubscription?.close('extension deactivated');
            reviewResponseSubscription = undefined;
            relayPool?.destroy();
            relayPool = undefined;
            activeRelayUrls = [];
            activeSubscriptionKey = undefined;
        })
    );

    void refreshRelaySubscription({ announceSession: false, notifyOnFailure: true });

    vscode.window.showInformationMessage('Drydock: Ready to review your code');
}

export function deactivate() {
    void reviewResponseSubscription?.close('extension deactivated');
    reviewResponseSubscription = undefined;
    relayPool?.destroy();
    relayPool = undefined;
    diagnosticCollection.dispose();
}

/**
 * Review uncommitted changes in the current workspace
 */
async function reviewChanges() {
    const workspaceFolder = vscode.workspace.workspaceFolders?.[0];
    if (!workspaceFolder) {
        vscode.window.showErrorMessage('No workspace folder open');
        return;
    }

    const workspacePath = workspaceFolder.uri.fsPath;

    try {
        // Get uncommitted diff
        const diff = execSync('git diff HEAD', {
            cwd: workspacePath,
            encoding: 'utf-8',
            maxBuffer: 10 * 1024 * 1024 // 10MB
        });

        if (!diff.trim()) {
            vscode.window.showInformationMessage('No uncommitted changes to review');
            return;
        }

        // Get list of changed files
        const changedFiles = execSync('git diff HEAD --name-only', {
            cwd: workspacePath,
            encoding: 'utf-8'
        }).trim().split('\n').filter(f => f);

        const config = getDrydockConfig();
        const privateKey = parsePrivateKey(config.privateKey);
        const drydockPubkey = tryParsePubkey(config.drydockPubkey);
        if (!drydockPubkey) {
            throw new Error('Configure drydock.drydockPubkey before requesting a review');
        }

        await refreshRelaySubscription({ announceSession: true, notifyOnFailure: true });

        if (!relayPool || activeRelayUrls.length === 0) {
            throw new Error('No Nostr relays configured');
        }

        // Build review request
        const requestId = uuidv4();
        const request: ReviewRequest = {
            session_id: sessionId,
            request_id: requestId,
            diff,
            changed_files: changedFiles,
            full_review: true
        };

        latestReviewRequestId = requestId;

        await vscode.window.withProgress({
            location: vscode.ProgressLocation.Notification,
            title: 'Drydock: Reviewing changes...',
            cancellable: false
        }, async () => {
            const rpcRequest = buildJSONRPCRequest(requestId, METHOD_REVIEW_REQUEST, request);
            const requestEvent = signEvent({
                kind: KIND_CONTEXTVM,
                content: JSON.stringify(rpcRequest),
                tags: buildContextVMRequestTags(config, requestId, METHOD_REVIEW_REQUEST)
            }, privateKey);

            await publishEvent(requestEvent);
        });

        vscode.window.showInformationMessage(
            `Drydock: Review request published to ${activeRelayUrls.length} relay(s)`
        );

        console.log('Review request:', JSON.stringify(request, null, 2));

    } catch (error) {
        const message = error instanceof Error ? error.message : String(error);
        vscode.window.showErrorMessage(`Drydock: Failed to submit review request: ${message}`);
    }
}

/**
 * Process review response and display diagnostics
 */
function handleReviewResponse(response: ReviewResponse) {
    const workspaceFolder = vscode.workspace.workspaceFolders?.[0];
    if (!workspaceFolder) return;

    if (response.session_id !== sessionId) {
        return;
    }

    if (!response.diagnostics.length && response.summary) {
        vscode.window.showWarningMessage(`Drydock: ${response.summary}`);
    }

    // Group diagnostics by file
    const diagnosticsByFile = new Map<string, vscode.Diagnostic[]>();

    pendingFixes.clear();
    pendingFixRequests.clear();

    for (const diag of response.diagnostics) {
        const filePath = vscode.Uri.joinPath(workspaceFolder.uri, diag.file);

        const range = new vscode.Range(
            diag.range.start_line,
            diag.range.start_column,
            diag.range.end_line,
            diag.range.end_column
        );

        const severity = convertSeverity(diag.severity);
        const diagnostic = new vscode.Diagnostic(range, diag.message, severity);
        diagnostic.source = diag.source || 'drydock';
        if (diag.code) {
            diagnostic.code = diag.code;
        }

        // Store fix information if available
        if (diag.has_fix && diag.fix_id) {
            pendingFixes.set(diag.fix_id, {
                file: diag.file,
                range,
                suggestedFix: diag.suggested_fix
            });
        }

        const fileKey = filePath.toString();
        if (!diagnosticsByFile.has(fileKey)) {
            diagnosticsByFile.set(fileKey, []);
        }
        diagnosticsByFile.get(fileKey)!.push(diagnostic);
    }

    // Update diagnostic collection
    diagnosticCollection.clear();
    for (const [file, diagnostics] of diagnosticsByFile) {
        diagnosticCollection.set(vscode.Uri.parse(file), diagnostics);
    }

    // Show summary
    const count = response.diagnostics.length;
    const timeMs = response.review_time_ms;
    const summarySuffix = response.summary ? ` — ${response.summary}` : '';
    vscode.window.showInformationMessage(
        `Drydock: Found ${count} issue(s) in ${timeMs}ms${summarySuffix}`
    );
}

/**
 * Convert numeric severity to VS Code DiagnosticSeverity
 */
function convertSeverity(severity: number): vscode.DiagnosticSeverity {
    switch (severity) {
        case 1: return vscode.DiagnosticSeverity.Error;
        case 2: return vscode.DiagnosticSeverity.Warning;
        case 3: return vscode.DiagnosticSeverity.Information;
        case 4: return vscode.DiagnosticSeverity.Hint;
        default: return vscode.DiagnosticSeverity.Information;
    }
}

interface DiffHunk {
    oldStart: number;
    oldCount: number;
    lines: string[];
}

function isUnifiedDiff(suggestedFix: string): boolean {
    return suggestedFix.split(/\r?\n/).some(line => line.startsWith('@@'));
}

function parseUnifiedDiffHunks(suggestedFix: string): DiffHunk[] {
    const lines = suggestedFix.split(/\r?\n/);
    const hunks: DiffHunk[] = [];
    const headerRegex = /^@@\s+-(\d+)(?:,(\d+))?\s+\+(\d+)(?:,(\d+))?\s+@@/;

    let index = 0;
    while (index < lines.length) {
        const line = lines[index];
        const match = line.match(headerRegex);
        if (!match) {
            index += 1;
            continue;
        }

        const oldStart = Number(match[1]);
        const oldCount = match[2] ? Number(match[2]) : 1;

        const hunkLines: string[] = [];
        index += 1;

        while (index < lines.length) {
            const hunkLine = lines[index];
            if (hunkLine.startsWith('@@')) {
                break;
            }

            if (
                hunkLine.startsWith(' ') ||
                hunkLine.startsWith('+') ||
                hunkLine.startsWith('-') ||
                hunkLine.startsWith('\\ No newline at end of file')
            ) {
                hunkLines.push(hunkLine);
            }

            index += 1;
        }

        hunks.push({ oldStart, oldCount, lines: hunkLines });
    }

    if (hunks.length === 0) {
        throw new Error('No diff hunks found in suggested fix');
    }

    return hunks;
}

function applyUnifiedDiffToText(originalText: string, hunks: DiffHunk[]): string {
    const normalizedOriginal = originalText.replace(/\r\n/g, '\n');
    const hasTrailingNewline = normalizedOriginal.endsWith('\n');
    const originalLines = normalizedOriginal.split('\n');
    if (hasTrailingNewline) {
        originalLines.pop();
    }

    const output: string[] = [];
    let cursor = 0;

    for (const hunk of hunks) {
        const hunkStart = Math.max(hunk.oldStart - 1, 0);
        if (hunkStart < cursor) {
            throw new Error('Overlapping or out-of-order diff hunks');
        }

        output.push(...originalLines.slice(cursor, hunkStart));

        let localCursor = hunkStart;
        let removedLines = 0;

        for (const line of hunk.lines) {
            if (line.startsWith('\\ No newline at end of file')) {
                continue;
            }

            const marker = line[0];
            const content = line.slice(1);

            if (marker === ' ') {
                if (originalLines[localCursor] !== content) {
                    throw new Error('Diff context does not match current file content');
                }
                output.push(content);
                localCursor += 1;
                continue;
            }

            if (marker === '-') {
                if (originalLines[localCursor] !== content) {
                    throw new Error('Diff deletion does not match current file content');
                }
                localCursor += 1;
                removedLines += 1;
                continue;
            }

            if (marker === '+') {
                output.push(content);
                continue;
            }

            throw new Error(`Unsupported diff line marker: ${marker}`);
        }

        if (removedLines !== hunk.oldCount) {
            // Keep processing even when oldCount metadata is approximate.
        }

        cursor = localCursor;
    }

    output.push(...originalLines.slice(cursor));

    const result = output.join('\n');
    return hasTrailingNewline ? `${result}\n` : result;
}

function getFullDocumentRange(document: vscode.TextDocument): vscode.Range {
    if (document.lineCount === 0) {
        return new vscode.Range(0, 0, 0, 0);
    }

    const lastLine = document.lineAt(document.lineCount - 1);
    return new vscode.Range(0, 0, document.lineCount - 1, lastLine.range.end.character);
}

function normalizeReplacementText(suggestedFix: string): string {
    const trimmed = suggestedFix.trim();
    const fencedMatch = trimmed.match(/^```(?:\w+)?\n([\s\S]*?)\n```$/);
    return fencedMatch ? fencedMatch[1] : suggestedFix;
}

function removeDiagnosticAtRange(uri: vscode.Uri, range: vscode.Range): void {
    const diagnostics = [...(diagnosticCollection.get(uri) ?? [])];
    const index = diagnostics.findIndex(diagnostic => diagnostic.range.isEqual(range));
    if (index === -1) {
        return;
    }

    diagnostics.splice(index, 1);
    if (diagnostics.length > 0) {
        diagnosticCollection.set(uri, diagnostics);
    } else {
        diagnosticCollection.delete(uri);
    }
}

/**
 * Apply a suggested fix
 */
async function applyFix() {
    const workspaceFolder = vscode.workspace.workspaceFolders?.[0];
    if (!workspaceFolder) {
        vscode.window.showErrorMessage('No workspace folder open');
        return;
    }

    const fixes = Array.from(pendingFixes.entries());
    if (fixes.length === 0) {
        vscode.window.showInformationMessage('No fixes available');
        return;
    }

    const items = fixes.map(([fixId, fix]) => ({
        label: `Fix in ${fix.file}`,
        description: fix.suggestedFix ? fix.suggestedFix.substring(0, 80) : fixId,
        fixId,
        fix
    }));

    const selected = await vscode.window.showQuickPick(items, {
        placeHolder: 'Select a fix to apply'
    });

    if (!selected) {
        return;
    }

    try {
        const config = getDrydockConfig();
        const privateKey = parsePrivateKey(config.privateKey);
        const drydockPubkey = tryParsePubkey(config.drydockPubkey);
        if (!drydockPubkey) {
            throw new Error('Configure drydock.drydockPubkey before requesting a fix');
        }

        await refreshRelaySubscription({ announceSession: true, notifyOnFailure: true });

        if (!relayPool || activeRelayUrls.length === 0) {
            throw new Error('No Nostr relays configured');
        }

        const requestId = uuidv4();
        const request: FixRequest = {
            session_id: sessionId,
            request_id: requestId,
            fix_id: selected.fixId,
            file: selected.fix.file
        };
        const rpcRequest = buildJSONRPCRequest(requestId, METHOD_APPLY_FIX, request);
        const requestEvent = signEvent({
            kind: KIND_CONTEXTVM,
            content: JSON.stringify(rpcRequest),
            tags: buildContextVMRequestTags(config, requestId, METHOD_APPLY_FIX)
        }, privateKey);

        pendingFixRequests.set(requestId, {
            fixId: selected.fixId,
            file: selected.fix.file,
            range: selected.fix.range
        });
        await publishEvent(requestEvent);
        vscode.window.showInformationMessage('Drydock: Fix request published');
    } catch (error) {
        const message = error instanceof Error ? error.message : String(error);
        vscode.window.showErrorMessage(`Failed to request fix: ${message}`);
    }
}

async function applySuggestedFix(file: string, range: vscode.Range, suggestedFix: string) {
    const workspaceFolder = vscode.workspace.workspaceFolders?.[0];
    if (!workspaceFolder) {
        vscode.window.showErrorMessage('No workspace folder open');
        return;
    }

    const fileUri = vscode.Uri.joinPath(workspaceFolder.uri, file);
    const document = await vscode.workspace.openTextDocument(fileUri);
    const edit = new vscode.WorkspaceEdit();

    if (isUnifiedDiff(suggestedFix)) {
        const hunks = parseUnifiedDiffHunks(suggestedFix);
        const updatedText = applyUnifiedDiffToText(document.getText(), hunks);
        edit.replace(fileUri, getFullDocumentRange(document), updatedText);
    } else {
        const replacement = normalizeReplacementText(suggestedFix);
        edit.replace(fileUri, range, replacement);
    }

    const applied = await vscode.workspace.applyEdit(edit);
    if (!applied) {
        throw new Error('VS Code rejected the workspace edit');
    }

    removeDiagnosticAtRange(fileUri, range);
}

/**
 * Clear all diagnostics
 */
function clearDiagnostics() {
    diagnosticCollection.clear();
    pendingFixes.clear();
    pendingFixRequests.clear();
    vscode.window.showInformationMessage('Drydock: Diagnostics cleared');
}

async function refreshRelaySubscription(options: { announceSession: boolean; notifyOnFailure: boolean }) {
    try {
        const config = getDrydockConfig();
        const relayUrls = normalizeRelayUrls(config.relays);

        if (relayUrls.length === 0) {
            await reviewResponseSubscription?.close('relay configuration empty');
            reviewResponseSubscription = undefined;
            activeRelayUrls = [];
            activeSubscriptionKey = undefined;
            relayPool?.destroy();
            relayPool = undefined;
            return;
        }

        relayPool ??= new SimplePool({ enablePing: true, enableReconnect: true });

        const subscriptionKey = JSON.stringify({
            relays: relayUrls,
            privateKey: config.privateKey,
            drydockPubkey: config.drydockPubkey,
            sessionId
        });

        if (activeSubscriptionKey !== subscriptionKey || !reviewResponseSubscription) {
            await reviewResponseSubscription?.close('refresh relay subscription');

            const filter: Filter = {
                kinds: [KIND_CONTEXTVM],
                since: Math.floor(Date.now() / 1000)
            };

            const authorPubkey = tryParsePubkey(config.drydockPubkey);
            if (authorPubkey) {
                filter.authors = [authorPubkey];
            }

            const clientPubkey = tryGetPublicKey(config.privateKey);
            if (clientPubkey) {
                filter['#p'] = [clientPubkey];
            }

            reviewResponseSubscription = relayPool.subscribe(relayUrls, filter, {
                onevent: (event: Event) => {
                    try {
                        handleIncomingReviewEvent(event);
                    } catch (error) {
                        console.error('Failed to process Drydock review response', error);
                    }
                },
                onclose: (reasons: string[]) => {
                    console.warn('Drydock review subscription closed', reasons);
                    reviewResponseSubscription = undefined;
                    activeSubscriptionKey = undefined;
                }
            });

            activeRelayUrls = relayUrls;
            activeSubscriptionKey = subscriptionKey;
        }

        if (options.announceSession) {
            await publishSessionAnnouncement(config);
        }
    } catch (error) {
        console.error('Failed to initialize Drydock relay subscription', error);
        if (options.notifyOnFailure) {
            const message = error instanceof Error ? error.message : String(error);
            vscode.window.showWarningMessage(`Drydock: Relay setup failed: ${message}`);
        }
        throw error;
    }
}

function handleIncomingReviewEvent(event: Event) {
    if (event.kind !== KIND_CONTEXTVM) {
        return;
    }

    const expectedAuthor = tryParsePubkey(getDrydockConfig().drydockPubkey);
    if (expectedAuthor && event.pubkey !== expectedAuthor) {
        return;
    }

    const response = JSON.parse(event.content) as JSONRPCResponse<ReviewResponse | FixResponse>;
    if (response.jsonrpc !== JSONRPC_VERSION) {
        return;
    }

    if (response.error) {
        if (response.id === latestReviewRequestId || pendingFixRequests.has(response.id)) {
            vscode.window.showWarningMessage(`Drydock: ${response.error.message}`);
            pendingFixRequests.delete(response.id);
        }
        return;
    }

    if (!response.result) {
        return;
    }

    if (isReviewResponse(response.result)) {
        if (response.result.session_id !== sessionId) {
            return;
        }

        if (latestReviewRequestId && response.id !== latestReviewRequestId && response.result.request_id !== latestReviewRequestId) {
            return;
        }

        handleReviewResponse(response.result);
        return;
    }

    if (isFixResponse(response.result)) {
        void handleFixResponse(response.id, response.result);
    }
}

function isReviewResponse(result: ReviewResponse | FixResponse): result is ReviewResponse {
    return 'diagnostics' in result && Array.isArray(result.diagnostics);
}

function isFixResponse(result: ReviewResponse | FixResponse): result is FixResponse {
    return 'fix_id' in result && 'success' in result;
}

async function handleFixResponse(responseId: string, response: FixResponse) {
    if (response.session_id && response.session_id !== sessionId) {
        return;
    }

    const pending = pendingFixRequests.get(responseId);
    if (!pending) {
        return;
    }
    pendingFixRequests.delete(responseId);

    if (!response.success) {
        vscode.window.showWarningMessage('Drydock: Fix was not available');
        return;
    }

    const fix = pendingFixes.get(response.fix_id) ?? pendingFixes.get(pending.fixId);
    const patch = response.patch ?? fix?.suggestedFix;
    if (!patch) {
        vscode.window.showWarningMessage('Drydock: Fix response did not include a patch');
        return;
    }

    try {
        await applySuggestedFix(pending.file, pending.range, patch);
        pendingFixes.delete(pending.fixId);
        vscode.window.showInformationMessage(`Applied fix in ${pending.file}`);
    } catch (error) {
        const message = error instanceof Error ? error.message : String(error);
        vscode.window.showErrorMessage(`Failed to apply fix: ${message}`);
    }
}

async function publishSessionAnnouncement(config: DrydockConfig) {
    if (!relayPool || activeRelayUrls.length === 0) {
        return;
    }

    const workspaceFolder = vscode.workspace.workspaceFolders?.[0];
    if (!workspaceFolder) {
        return;
    }

    if (!config.privateKey.trim()) {
        return;
    }

    const privateKey = parsePrivateKey(config.privateKey);
    const announcement: IDESessionAnnouncement = {
        session_id: sessionId,
        workspace_path: workspaceFolder.uri.fsPath,
        repo_id: '',
        editor: 'vscode',
        version: extensionVersion,
        languages: getWorkspaceLanguages()
    };

    const sessionEvent = signEvent({
        kind: KIND_IDE_SESSION,
        content: JSON.stringify(announcement),
        tags: [
            ['d', `drydock:ide-session:${sessionId}`],
            ['type', 'ide-session'],
            ['schema', IDE_SESSION_SCHEMA],
            ['client', `vscode-drydock/${extensionVersion}`]
        ]
    }, privateKey);

    await publishEvent(sessionEvent);
}

async function publishEvent(event: VerifiedEvent) {
    if (!relayPool || activeRelayUrls.length === 0) {
        throw new Error('No active relay connections');
    }

    const publishResults = await Promise.allSettled(relayPool.publish(activeRelayUrls, event));
    const publishOutcomes = publishResults.map(result => {
        if (result.status === 'fulfilled') {
            const message = String(result.value);
            return message.startsWith('connection failure: ')
                ? { ok: false, message }
                : { ok: true, message };
        }

        return { ok: false, message: String(result.reason) };
    });

    if (publishOutcomes.some(result => result.ok)) {
        return;
    }

    const reasons = publishOutcomes.map(result => result.message).join('; ');
    throw new Error(`Publish failed on all relays: ${reasons || 'unknown error'}`);
}

function signEvent(template: Omit<EventTemplate, 'created_at'>, privateKey: Uint8Array): VerifiedEvent {
    return finalizeEvent({
        ...template,
        created_at: Math.floor(Date.now() / 1000)
    }, privateKey);
}

function getDrydockConfig(): DrydockConfig {
    const config = vscode.workspace.getConfiguration('drydock');
    return {
        relays: config.get<string[]>('relays', []),
        privateKey: config.get<string>('privateKey', '').trim(),
        drydockPubkey: config.get<string>('drydockPubkey', '').trim()
    };
}

function normalizeRelayUrls(relays: string[]): string[] {
    return Array.from(new Set(
        relays
            .map(relay => relay.trim())
            .filter(relay => relay.length > 0)
    ));
}

function parsePrivateKey(value: string): Uint8Array {
    const trimmed = value.trim();
    if (!trimmed) {
        throw new Error('Configure drydock.privateKey before requesting a review');
    }

    if (trimmed.startsWith('nsec1')) {
        const decoded = nip19.decode(trimmed);
        if (decoded.type !== 'nsec') {
            throw new Error('drydock.privateKey must be an nsec or 64-character hex key');
        }
        return decoded.data;
    }

    if (!/^[0-9a-fA-F]{64}$/.test(trimmed)) {
        throw new Error('drydock.privateKey must be an nsec or 64-character hex key');
    }

    return Uint8Array.from(Buffer.from(trimmed, 'hex'));
}

function tryGetPublicKey(privateKey: string): string | undefined {
    try {
        return getPublicKey(parsePrivateKey(privateKey));
    } catch {
        return undefined;
    }
}

function tryParsePubkey(value: string): string | undefined {
    const trimmed = value.trim();
    if (!trimmed) {
        return undefined;
    }

    if (/^[0-9a-fA-F]{64}$/.test(trimmed)) {
        return trimmed.toLowerCase();
    }

    if (trimmed.startsWith('npub1')) {
        const decoded = nip19.decode(trimmed);
        if (decoded.type === 'npub') {
            return decoded.data;
        }
    }

    throw new Error('drydock.drydockPubkey must be an npub or 64-character hex public key');
}

function buildJSONRPCRequest<T>(id: string, method: string, params: T): JSONRPCRequest<T> {
    return {
        jsonrpc: JSONRPC_VERSION,
        id,
        method,
        params
    };
}

function buildContextVMRequestTags(config: DrydockConfig, requestId: string, method: string): string[][] {
    const tags: string[][] = [
        ['session', sessionId],
        ['request', requestId],
        ['method', method]
    ];

    const drydockPubkey = tryParsePubkey(config.drydockPubkey);
    if (drydockPubkey) {
        tags.push(['p', drydockPubkey]);
    }

    return tags;
}

function getWorkspaceLanguages(): string[] {
    return Array.from(new Set(
        vscode.workspace.textDocuments
            .map(document => document.languageId)
            .filter(languageId => languageId && languageId !== 'Log')
    ));
}
