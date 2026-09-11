import type {Metadata} from 'next';
import {Inter, Noto_Sans_SC} from 'next/font/google';
import {Toaster} from '@/components/ui/sonner';
import {ThemeProvider} from '@/components/common/layout/ThemeProvider';
import {AuthProvider} from '@/lib/auth-context';
import './globals.css';

const inter = Inter({
  variable: '--font-inter',
  subsets: ['latin'],
  display: 'swap',
});

const notoSansSC = Noto_Sans_SC({
  variable: '--font-noto-sans-sc',
  subsets: ['latin'],
  display: 'swap',
  weight: ['300', '400', '500', '600', '700'],
});

export const metadata: Metadata = {
  title: {
    template: '%s - WorkBuddy Manager',
    default: 'WorkBuddy Manager',
  },
  description: 'WorkBuddy Manager - 腾讯 CodeBuddy 账号池管理与 OpenAI 兼容反代网关',
};

export default function RootLayout({
  children,
}: Readonly<{
  children: React.ReactNode;
}>) {
  return (
    <html
      lang="zh-CN"
      className={`${inter.variable} ${notoSansSC.variable} hide-scrollbar font-sans`}
      suppressHydrationWarning
    >
      <body
        className={`${inter.variable} ${notoSansSC.variable} hide-scrollbar font-sans antialiased`}
      >
        <ThemeProvider
          attribute="class"
          defaultTheme="system"
          enableSystem
          disableTransitionOnChange
        >
          <AuthProvider>
            {children}
            <Toaster />
          </AuthProvider>
        </ThemeProvider>
      </body>
    </html>
  );
}
