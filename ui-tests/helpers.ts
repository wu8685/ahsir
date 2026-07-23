import {
  APIRequestContext,
  BrowserContext,
  Page,
  expect,
  test as base,
} from '@playwright/test';
import { writeFile } from 'node:fs/promises';

type ConsoleError = {
  locationURL: string;
  text: string;
};

type FailedResponse = {
  status: number;
  statusText: string;
  url: string;
};

type RequestFailure = {
  errorText: string;
  url: string;
};

type ExternalRequest = {
  method: string;
  resourceType: string;
  url: string;
};

export async function resetScenario(request: APIRequestContext, scenario: string) {
  const response = await request.post(`/__test/reset?scenario=${encodeURIComponent(scenario)}`);
  expect(response.status()).toBe(204);
}

export class BrowserDiagnostics {
  private readonly allowedFailedResponses = new Set<string>();
  private readonly consoleErrors: ConsoleError[] = [];
  private readonly externalRequests: ExternalRequest[] = [];
  private readonly failedResponses: FailedResponse[] = [];
  private readonly pageErrors: string[] = [];
  private readonly requestFailures: RequestFailure[] = [];

  constructor(page: Page, private readonly baseURL: string) {
    page.on('pageerror', error => this.pageErrors.push(error.message));
    page.on('console', message => {
      if (message.type() !== 'error') return;
      this.consoleErrors.push({
        locationURL: message.location().url,
        text: message.text(),
      });
    });
    page.on('response', response => {
      if (response.status() < 400) return;
      this.failedResponses.push({
        status: response.status(),
        statusText: response.statusText(),
        url: response.url(),
      });
    });
    page.on('requestfailed', request => {
      this.requestFailures.push({
        errorText: request.failure()?.errorText ?? 'unknown request failure',
        url: request.url(),
      });
    });
  }

  async installNetworkGuard(context: BrowserContext) {
    const allowedOrigin = new URL(this.baseURL).origin;
    await context.route(/^https?:\/\//, async route => {
      const request = route.request();
      if (new URL(request.url()).origin === allowedOrigin) {
        await route.continue();
        return;
      }
      this.externalRequests.push({
        method: request.method(),
        resourceType: request.resourceType(),
        url: request.url(),
      });
      await route.abort('blockedbyclient');
    });
  }

  allowFailedResponse(path: string, status: number) {
    this.allowedFailedResponses.add(responseKey(new URL(path, this.baseURL).href, status));
  }

  snapshot() {
    return {
      allowedFailedResponses: [...this.allowedFailedResponses].sort(),
      consoleErrors: this.consoleErrors,
      externalRequests: this.externalRequests,
      failedResponses: this.failedResponses,
      pageErrors: this.pageErrors,
      requestFailures: this.requestFailures,
    };
  }

  unexpectedErrors() {
    const errors = [
      ...this.pageErrors.map(error => `pageerror: ${error}`),
      ...this.externalRequests.map(
        request =>
          `external HTTP(S) request blocked: ${request.method} ${request.url} (${request.resourceType})`,
      ),
      ...this.requestFailures
        .filter(failure => !this.isBlockedExternalURL(failure.url))
        .map(failure => `requestfailed: ${failure.url}: ${failure.errorText}`),
      ...this.failedResponses
        .filter(
          response =>
            !this.isBlockedExternalURL(response.url) && !this.isAllowedResponse(response),
        )
        .map(response => `response: ${response.status} ${response.url}`),
      ...this.consoleErrors
        .filter(error => !this.isExpectedResourceError(error))
        .map(error => `console: ${error.text} (${error.locationURL || 'no location'})`),
    ];
    return errors;
  }

  private isAllowedResponse(response: FailedResponse) {
    return this.allowedFailedResponses.has(responseKey(response.url, response.status));
  }

  private isExpectedResourceError(error: ConsoleError) {
    if (!error.text.startsWith('Failed to load resource:')) return false;
    if (this.isBlockedExternalURL(error.locationURL)) return true;
    return this.failedResponses.some(
      response =>
        response.url === error.locationURL &&
        this.isAllowedResponse(response),
    );
  }

  private isBlockedExternalURL(url: string) {
    return this.externalRequests.some(request => request.url === url);
  }
}

function responseKey(url: string, status: number) {
  return `${status} ${url}`;
}

export function collectPageErrors(page: Page, baseURL: string) {
  return new BrowserDiagnostics(page, baseURL);
}

type DiagnosticsFixtures = {
  browserDiagnostics: BrowserDiagnostics;
};

export const test = base.extend<DiagnosticsFixtures>({
  browserDiagnostics: [
    async ({ baseURL, context, page }, use) => {
      if (!baseURL) throw new Error('Playwright baseURL is required for browser diagnostics');
      const diagnostics = collectPageErrors(page, baseURL);
      await diagnostics.installNetworkGuard(context);
      await use(diagnostics);
    },
    { auto: true },
  ],
});

test.afterEach(async ({ browserDiagnostics }, testInfo) => {
  const unexpectedErrors = browserDiagnostics.unexpectedErrors();
  if (testInfo.status !== testInfo.expectedStatus || unexpectedErrors.length > 0) {
    const diagnosticsPath = testInfo.outputPath('browser-diagnostics.json');
    await writeFile(diagnosticsPath, JSON.stringify(browserDiagnostics.snapshot(), null, 2));
    await testInfo.attach('browser-diagnostics.json', {
      path: diagnosticsPath,
      contentType: 'application/json',
    });
  }
  expect(unexpectedErrors, 'unexpected browser diagnostics').toEqual([]);
});

export { expect };
