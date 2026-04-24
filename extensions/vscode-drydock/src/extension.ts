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

// Nostr event kinds for IDE integration
const KIND_IDE_SESSION = 31650;
const KIND_IDE_REVIEW_REQUEST = 1651;
const KIND_IDE_REVIEW_RESPONSE = 1652;
const KIND_IDE_FIX_REQUEST = 1653;
const KIND_IDE_FIX_RESPONSE = 1654;

// Diagnostic collection for displaying findings
let diagnosticCollection: vscode.DiagnosticCollection;

// Session state
let sessionId: string;

// Store pending fixes by ID
const pendingFixes: Map<string, PendingFix> = new Map();

interface PendingFix {
    file: string;
    range: vscode.Range;
    suggestedFix: string;
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

export function activate(context: vscode.ExtensionContext) {
    console.log('Drydock extension activated');

    // Create diagnostic collection
    diagnosticCollection = vscode.languages.createDiagnosticCollection('drydock');
    context.subscriptions.push(diagnosticCollection);

    // Generate session ID
    sessionId = uuidv4();

    // Register commands
    context.subscriptions.push(
        vscode.commands.registerCommand('drydock.reviewChanges', reviewChanges),
        vscode.commands.registerCommand('drydock.applyFix', applyFix),
        vscode.commands.registerCommand('drydock.clearDiagnostics', clearDiagnostics)
    );

    // TODO: Connect to Nostr relays and subscribe to responses
    // This is a simplified implementation that shows the structure
    // Full implementation would use nostr-tools for relay communication

    vscode.window.showInformationMessage('Drydock: Ready to review your code');
}

export function deactivate() {
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

        // Build review request
        const requestId = uuidv4();
        const request = {
            session_id: sessionId,
            request_id: requestId,
            diff: diff,
            changed_files: changedFiles,
            full_review: true
        };

        vscode.window.withProgress({
            location: vscode.ProgressLocation.Notification,
            title: 'Drydock: Reviewing changes...',
            cancellable: false
        }, async () => {
            // In full implementation, this would:
            // 1. Sign the event with user's Nostr key
            // 2. Publish to configured relays
            // 3. Subscribe to response events
            // 4. Process responses when received
            
            // For now, show a placeholder message
            await new Promise(resolve => setTimeout(resolve, 1000));
            vscode.window.showInformationMessage(
                `Drydock: Review request sent (${changedFiles.length} files). ` +
                'Awaiting response from Nostr...'
            );
        });

        console.log('Review request:', JSON.stringify(request, null, 2));

    } catch (error) {
        vscode.window.showErrorMessage(`Drydock: Failed to get diff: ${error}`);
    }
}

/**
 * Process review response and display diagnostics
 */
function handleReviewResponse(response: ReviewResponse) {
    const workspaceFolder = vscode.workspace.workspaceFolders?.[0];
    if (!workspaceFolder) return;

    // Group diagnostics by file
    const diagnosticsByFile = new Map<string, vscode.Diagnostic[]>();

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
        if (diag.has_fix && diag.fix_id && diag.suggested_fix) {
            pendingFixes.set(diag.fix_id, {
                file: diag.file,
                range: range,
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
    vscode.window.showInformationMessage(
        `Drydock: Found ${count} issue(s) in ${timeMs}ms`
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

/**
 * Apply a suggested fix
 */
async function applyFix() {
    // In a full implementation, this would:
    // 1. Get the fix ID from the current diagnostic
    // 2. Look up the fix in pendingFixes
    // 3. Apply the diff to the file
    // 4. Send a fix request to Drydock to confirm
    
    const fixes = Array.from(pendingFixes.values());
    if (fixes.length === 0) {
        vscode.window.showInformationMessage('No fixes available');
        return;
    }

    const items = fixes.map((fix, i) => ({
        label: `Fix in ${fix.file}`,
        description: fix.suggestedFix.substring(0, 50) + '...',
        index: i
    }));

    const selected = await vscode.window.showQuickPick(items, {
        placeHolder: 'Select a fix to apply'
    });

    if (selected) {
        vscode.window.showInformationMessage(
            `Would apply fix: ${selected.label}`
        );
    }
}

/**
 * Clear all diagnostics
 */
function clearDiagnostics() {
    diagnosticCollection.clear();
    pendingFixes.clear();
    vscode.window.showInformationMessage('Drydock: Diagnostics cleared');
}
