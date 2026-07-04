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
import { createHash } from 'crypto';
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

// Nostr event kinds and ContextVM methods for IDE integration
const KIND_IDE_SESSION = 30078;
const KIND_CONTEXTVM = 25910;
const METHOD_IDE_REVIEW = 'ide/review';
const METHOD_IDE_APPLY_FIX = 'ide/applyFix';
const IDE_CONTEXT_TAG = 'drydock-ide';

const PRIVATE_KEY_SECRET_KEY = 'drydock.privateKey';
const PUBLIC_DIFF_WARNING_ACTION = 'Publish Diff';

// Diagnostic collection for displaying findings
let diagnosticCollection: vscode.DiagnosticCollection;
let secretStorage: vscode.SecretStorage | undefined;

// Session state
let sessionId: string;
let extensionVersion = '0.0.0';

// Relay state
let relayPool: SimplePool | undefined;
let reviewResponseSubscription: { close: (reason?: string) => void | Promise<void> } | undefined;
let activeRelayUrls: string[] = [];
let activeSubscriptionKey: string | undefined;
let latestReviewRequestId: string | undefined;
let activeDrydockPubkey: string | undefined;
let autoReviewInFlight = false;

// Store pending fixes by ID
const pendingFixes: Map<string, PendingFix> = new Map();
const pendingFixResponses: Map<string, {
    resolve: (response: FixResponse) => void;
    reject: (error: Error) => void;
}> = new Map();

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

interface FixRequest {
    session_id: string;
    fix_id: string;
    file: string;
}

interface FixResponse {
    request_id: string;
    session_id: string;
    success: boolean;
    diff?: string;
    error?: string;
}

interface ContextVMRequest<T> {
    jsonrpc: '2.0';
    id: string;
    method: string;
    params: T;
}

interface ContextVMResponse<T> {
    jsonrpc?: string;
    id: string;
    result?: T;
    error?: {
        code: number;
        message: string;
        data?: unknown;
    };
}

interface IDESessionAnnouncement {
    session_id: string;
    workspace_path: string;
    repo_id: string;
    repo_ref: string;
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

interface DrydockConfig {
    relays: string[];
    privateKey: string;
    drydockPubkey: string;
}

export function activate(context: vscode.ExtensionContext) {
    console.log('Drydock extension activated');

    extensionVersion = String(context.extension.packageJSON.version ?? '0.0.0');
    secretStorage = context.secrets;

    // Create diagnostic collection
    diagnosticCollection = vscode.languages.createDiagnosticCollection('drydock');
    context.subscriptions.push(diagnosticCollection);

    // Generate session ID
    sessionId = uuidv4();

    // Register commands
    context.subscriptions.push(
        vscode.commands.registerCommand('drydock.reviewChanges', () => reviewChanges()),
        vscode.commands.registerCommand('drydock.applyFix', applyFix),
        vscode.commands.registerCommand('drydock.clearDiagnostics', clearDiagnostics),
        vscode.commands.registerCommand('drydock.setPrivateKey', () => setPrivateKey(context)),
        vscode.workspace.onDidSaveTextDocument(document => {
            void maybeAutoReview(document);
        }),
        vscode.workspace.onDidChangeConfiguration(event => {
            if (event.affectsConfiguration('drydock.relays') ||
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
            activeDrydockPubkey = undefined;
            rejectPendingFixResponses(new Error('Extension deactivated'));
        })
    );

    void migratePrivateKeySetting(context).finally(() => {
        void refreshRelaySubscription({ announceSession: false, notifyOnFailure: true });
    });

    vscode.window.showInformationMessage('Drydock: Ready to review your code');
}

export function deactivate() {
    void reviewResponseSubscription?.close('extension deactivated');
    reviewResponseSubscription = undefined;
    relayPool?.destroy();
    relayPool = undefined;
    activeRelayUrls = [];
    activeSubscriptionKey = undefined;
    activeDrydockPubkey = undefined;
    rejectPendingFixResponses(new Error('Extension deactivated'));
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

        const config = await getDrydockConfig();
        const privateKey = parsePrivateKey(config.privateKey);
        const drydockPubkey = tryParsePubkey(config.drydockPubkey);
        if (!drydockPubkey) {
            throw new Error('Configure drydock.drydockPubkey before requesting a review');
        }

        if (!(await confirmSourceDiffPublish(config.relays))) {
            return;
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
            const requestEvent = signEvent({
                kind: KIND_CONTEXTVM,
                content: JSON.stringify(buildContextVMRequest(requestId, METHOD_IDE_REVIEW, request)),
                tags: buildContextVMRequestTags(config, requestId, METHOD_IDE_REVIEW)
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

        // Store fix information if available. The suggested fix may be present in
        // the review response, but apply still retrieves the authoritative fix via
        // the ContextVM ide/applyFix method.
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
        description: (fix.suggestedFix ?? 'Retrieve fix from Drydock').substring(0, 80),
        fixId,
        fix
    }));

    const selected = await vscode.window.showQuickPick(items, {
        placeHolder: 'Select a fix to apply'
    });

    if (!selected) {
        return;
    }

    const fileUri = vscode.Uri.joinPath(workspaceFolder.uri, selected.fix.file);

    try {
        const fixResponse = await vscode.window.withProgress({
            location: vscode.ProgressLocation.Notification,
            title: 'Drydock: Retrieving fix...',
            cancellable: false
        }, () => requestFix(selected.fixId, selected.fix.file));

        const suggestedFix = fixResponse.diff || selected.fix.suggestedFix;
        if (!suggestedFix) {
            throw new Error(fixResponse.error || 'Drydock returned an empty fix');
        }

        const document = await vscode.workspace.openTextDocument(fileUri);
        const edit = new vscode.WorkspaceEdit();

        if (isUnifiedDiff(suggestedFix)) {
            const hunks = parseUnifiedDiffHunks(suggestedFix);
            const updatedText = applyUnifiedDiffToText(document.getText(), hunks);
            edit.replace(fileUri, getFullDocumentRange(document), updatedText);
        } else {
            const replacement = normalizeReplacementText(suggestedFix);
            edit.replace(fileUri, selected.fix.range, replacement);
        }

        const applied = await vscode.workspace.applyEdit(edit);
        if (!applied) {
            throw new Error('VS Code rejected the workspace edit');
        }

        pendingFixes.delete(selected.fixId);
        removeDiagnosticAtRange(fileUri, selected.fix.range);

        vscode.window.showInformationMessage(`Applied fix in ${selected.fix.file}`);
    } catch (error) {
        const message = error instanceof Error ? error.message : String(error);
        vscode.window.showErrorMessage(`Failed to apply fix: ${message}`);
    }
}

async function requestFix(fixId: string, file: string): Promise<FixResponse> {
    const config = await getDrydockConfig();
    const privateKey = parsePrivateKey(config.privateKey);
    const drydockPubkey = tryParsePubkey(config.drydockPubkey);
    if (!drydockPubkey) {
        throw new Error('Configure drydock.drydockPubkey before requesting a fix');
    }

    await refreshRelaySubscription({ announceSession: false, notifyOnFailure: true });
    if (!relayPool || activeRelayUrls.length === 0) {
        throw new Error('No Nostr relays configured');
    }

    const requestId = uuidv4();
    const request: FixRequest = {
        session_id: sessionId,
        fix_id: fixId,
        file
    };

    const responsePromise = new Promise<FixResponse>((resolve, reject) => {
        pendingFixResponses.set(requestId, { resolve, reject });
    });

    try {
        const requestEvent = signEvent({
            kind: KIND_CONTEXTVM,
            content: JSON.stringify(buildContextVMRequest(requestId, METHOD_IDE_APPLY_FIX, request)),
            tags: buildContextVMRequestTags(config, requestId, METHOD_IDE_APPLY_FIX)
        }, privateKey);
        await publishEvent(requestEvent);
    } catch (error) {
        pendingFixResponses.delete(requestId);
        throw error;
    }

    return responsePromise;
}

/**
 * Clear all diagnostics
 */
function clearDiagnostics() {
    diagnosticCollection.clear();
    pendingFixes.clear();
    vscode.window.showInformationMessage('Drydock: Diagnostics cleared');
}

function rejectPendingFixResponses(error: Error): void {
    for (const pending of pendingFixResponses.values()) {
        pending.reject(error);
    }
    pendingFixResponses.clear();
}

async function refreshRelaySubscription(options: { announceSession: boolean; notifyOnFailure: boolean }) {
    try {
        const config = await getDrydockConfig();
        const relayUrls = normalizeRelayUrls(config.relays);

        if (relayUrls.length === 0) {
            await reviewResponseSubscription?.close('relay configuration empty');
            reviewResponseSubscription = undefined;
            activeRelayUrls = [];
            activeSubscriptionKey = undefined;
            activeDrydockPubkey = undefined;
            relayPool?.destroy();
            relayPool = undefined;
            return;
        }

        relayPool ??= new SimplePool({ enablePing: true, enableReconnect: true });

        const clientPubkey = tryGetPublicKey(config.privateKey);
        const authorPubkey = tryParsePubkey(config.drydockPubkey);
        activeDrydockPubkey = authorPubkey;

        const subscriptionKey = JSON.stringify({
            relays: relayUrls,
            clientPubkey,
            drydockPubkey: config.drydockPubkey,
            sessionId
        });

        if (activeSubscriptionKey !== subscriptionKey || !reviewResponseSubscription) {
            await reviewResponseSubscription?.close('refresh relay subscription');

            const filter: Filter = {
                kinds: [KIND_CONTEXTVM],
                since: Math.floor(Date.now() / 1000)
            };

            if (authorPubkey) {
                filter.authors = [authorPubkey];
            }

            if (clientPubkey) {
                filter['#p'] = [clientPubkey];
            }
            filter['#t'] = [IDE_CONTEXT_TAG];

            reviewResponseSubscription = relayPool.subscribe(relayUrls, filter, {
                onevent: (event: Event) => {
                    try {
                        handleIncomingContextVMEvent(event);
                    } catch (error) {
                        console.error('Failed to process Drydock ContextVM response', error);
                    }
                },
                onclose: (reasons: string[]) => {
                    console.warn('Drydock review subscription closed', reasons);
                    reviewResponseSubscription = undefined;
                    activeSubscriptionKey = undefined;
                    rejectPendingFixResponses(new Error(`Drydock relay subscription closed: ${reasons.join('; ')}`));
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

function handleIncomingContextVMEvent(event: Event) {
    if (event.kind !== KIND_CONTEXTVM) {
        return;
    }

    const expectedAuthor = activeDrydockPubkey;
    if (expectedAuthor && event.pubkey !== expectedAuthor) {
        return;
    }

    const envelope = JSON.parse(event.content) as ContextVMResponse<unknown>;
    if (!envelope.id) {
        return;
    }

    const pendingFix = pendingFixResponses.get(envelope.id);
    if (pendingFix) {
        pendingFixResponses.delete(envelope.id);
        if (envelope.error) {
            pendingFix.reject(new Error(envelope.error.message));
            return;
        }
        const response = envelope.result as FixResponse | undefined;
        if (!response) {
            pendingFix.reject(new Error('Drydock returned an empty fix response'));
            return;
        }
        if (response.session_id !== sessionId) {
            pendingFix.reject(new Error('Drydock fix response was for a different session'));
            return;
        }
        if (!response.success) {
            pendingFix.reject(new Error(response.error || 'Drydock could not resolve the fix'));
            return;
        }
        pendingFix.resolve(response);
        return;
    }

    if (latestReviewRequestId && envelope.id !== latestReviewRequestId) {
        return;
    }
    if (envelope.error) {
        vscode.window.showWarningMessage(`Drydock: ${envelope.error.message}`);
        return;
    }

    const response = envelope.result as ReviewResponse | undefined;
    if (!response || response.session_id !== sessionId || !Array.isArray(response.diagnostics)) {
        return;
    }

    handleReviewResponse(response);
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
    const drydockPubkey = tryParsePubkey(config.drydockPubkey);
    if (!drydockPubkey) {
        return;
    }

    const repoIdentity = getRepoIdentity(workspaceFolder);
    const announcement: IDESessionAnnouncement = {
        session_id: sessionId,
        workspace_path: workspaceFolder.uri.fsPath,
        repo_id: repoIdentity.repoID,
        repo_ref: repoIdentity.repoRef,
        editor: 'vscode',
        version: extensionVersion,
        languages: getWorkspaceLanguages()
    };

    const sessionEvent = signEvent({
        kind: KIND_IDE_SESSION,
        content: JSON.stringify(announcement),
        tags: [
            ['d', sessionId],
            ['p', drydockPubkey],
            ['repo', repoIdentity.repoID],
            ['r', repoIdentity.repoRef],
            ['t', IDE_CONTEXT_TAG]
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

async function getDrydockConfig(): Promise<DrydockConfig> {
    const config = vscode.workspace.getConfiguration('drydock');
    return {
        relays: config.get<string[]>('relays', []),
        privateKey: (await secretStorage?.get(PRIVATE_KEY_SECRET_KEY) ?? '').trim(),
        drydockPubkey: config.get<string>('drydockPubkey', '').trim()
    };
}

async function setPrivateKey(context: vscode.ExtensionContext): Promise<void> {
    const value = await vscode.window.showInputBox({
        prompt: 'Enter your Nostr private key. It will be stored in VS Code SecretStorage, not settings.',
        placeHolder: 'nsec1... or 64-character hex key; leave empty to clear',
        password: true,
        ignoreFocusOut: true
    });
    if (value === undefined) {
        return;
    }

    const trimmed = value.trim();
    if (!trimmed) {
        await context.secrets.delete(PRIVATE_KEY_SECRET_KEY);
        vscode.window.showInformationMessage('Drydock: Private key cleared from SecretStorage');
        await refreshRelaySubscription({ announceSession: false, notifyOnFailure: true });
        return;
    }

    try {
        parsePrivateKey(trimmed);
    } catch (error) {
        const message = error instanceof Error ? error.message : String(error);
        vscode.window.showErrorMessage(`Drydock: Invalid private key: ${message}`);
        return;
    }

    await context.secrets.store(PRIVATE_KEY_SECRET_KEY, trimmed);
    await clearLegacyPrivateKeySetting();
    vscode.window.showInformationMessage('Drydock: Private key stored securely');
    await refreshRelaySubscription({ announceSession: false, notifyOnFailure: true });
}

async function migratePrivateKeySetting(context: vscode.ExtensionContext): Promise<void> {
    const config = vscode.workspace.getConfiguration('drydock');
    const legacyPrivateKey = config.get<string>('privateKey', '').trim();
    if (!legacyPrivateKey) {
        return;
    }

    const existingSecret = await context.secrets.get(PRIVATE_KEY_SECRET_KEY);
    if (!existingSecret) {
        await context.secrets.store(PRIVATE_KEY_SECRET_KEY, legacyPrivateKey);
        vscode.window.showWarningMessage('Drydock: Migrated drydock.privateKey from settings to VS Code SecretStorage. The plaintext setting was cleared.');
    }
    await clearLegacyPrivateKeySetting();
}

async function clearLegacyPrivateKeySetting(): Promise<void> {
    const config = vscode.workspace.getConfiguration('drydock');
    for (const target of [vscode.ConfigurationTarget.Global, vscode.ConfigurationTarget.Workspace]) {
        try {
            await config.update('privateKey', undefined, target);
        } catch (error) {
            console.warn('Failed to clear legacy drydock.privateKey setting', error);
        }
    }
}

async function maybeAutoReview(document: vscode.TextDocument): Promise<void> {
    if (document.uri.scheme !== 'file') {
        return;
    }
    const config = vscode.workspace.getConfiguration('drydock');
    if (!config.get<boolean>('autoReview', false)) {
        return;
    }
    if (!vscode.workspace.getWorkspaceFolder(document.uri)) {
        return;
    }
    if (autoReviewInFlight) {
        return;
    }

    autoReviewInFlight = true;
    try {
        await reviewChanges();
    } finally {
        autoReviewInFlight = false;
    }
}

async function confirmSourceDiffPublish(relays: string[]): Promise<boolean> {
    const publicRelays = normalizeRelayUrls(relays).filter(isLikelyPublicRelay);
    if (publicRelays.length === 0) {
        return true;
    }

    const selected = await vscode.window.showWarningMessage(
        `Drydock review requests include your uncommitted source diff. These relay(s) look public: ${publicRelays.join(', ')}. Only continue if you trust them.`,
        { modal: true },
        PUBLIC_DIFF_WARNING_ACTION
    );
    return selected === PUBLIC_DIFF_WARNING_ACTION;
}

function isLikelyPublicRelay(relay: string): boolean {
    try {
        const url = new URL(relay);
        const host = url.hostname.toLowerCase();
        if (host === 'localhost' || host === '::1' || host.endsWith('.local')) {
            return false;
        }
        if (/^127\./.test(host) || /^10\./.test(host) || /^192\.168\./.test(host)) {
            return false;
        }
        const private172 = host.match(/^172\.(\d+)\./);
        if (private172) {
            const secondOctet = Number(private172[1]);
            if (secondOctet >= 16 && secondOctet <= 31) {
                return false;
            }
        }
        return true;
    } catch {
        return true;
    }
}

function getRepoIdentity(workspaceFolder: vscode.WorkspaceFolder): { repoID: string; repoRef: string } {
    const hashInput = getGitRemote(workspaceFolder.uri.fsPath) || `workspace:${workspaceFolder.name}`;
    const repoID = createHash('sha256').update(hashInput).digest('hex');
    return {
        repoID,
        repoRef: `repo:${repoID}`
    };
}

function getGitRemote(workspacePath: string): string | undefined {
    try {
        const remote = execSync('git config --get remote.origin.url', {
            cwd: workspacePath,
            encoding: 'utf-8',
            stdio: ['ignore', 'pipe', 'ignore']
        }).trim();
        return remote || undefined;
    } catch {
        return undefined;
    }
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
        throw new Error('Store a Nostr private key with the "Drydock: Store Nostr Private Key" command before requesting a review');
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

function buildContextVMRequest<T>(id: string, method: string, params: T): ContextVMRequest<T> {
    return {
        jsonrpc: '2.0',
        id,
        method,
        params
    };
}

function buildContextVMRequestTags(config: DrydockConfig, requestId: string, method: string): string[][] {
    const tags: string[][] = [
        ['session', sessionId],
        ['request', requestId],
        ['method', method],
        ['t', IDE_CONTEXT_TAG]
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
