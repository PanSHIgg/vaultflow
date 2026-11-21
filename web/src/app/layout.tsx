import type { Metadata } from "next";
import type { ReactNode } from "react";
import { Providers } from "./providers";
import "./globals.css";

export const metadata: Metadata = {
  title: "VaultFlow",
  description: "VaultFlow 数字资产充值",
};

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="zh-CN">
      <body className="min-h-screen bg-[#0B0E11] text-[#EAECEF] antialiased">
        <Providers>{children}</Providers>
      </body>
    </html>
  );
}
